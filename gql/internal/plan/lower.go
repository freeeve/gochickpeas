// Core lowering primitives (port of the Rust plan/lower.rs): slot
// assignment, anchor seeks (id/text-match recognition), scan-source
// selection, hop building, var-length hop-predicate extraction, projection
// binding, and the shared AND-chain helpers.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// assignSlot resolves a node pattern's slot: an existing variable keeps its
// slot (wasBound reports whether it is already bound); a new or anonymous
// node takes the next slot.
func assignSlot(node *ast.NodePat, slots map[string]int, bound map[int]bool, nextSlot *int) (slot int, wasBound bool) {
	if node.Var != "" {
		if s, ok := slots[node.Var]; ok {
			return s, bound[s]
		}
		s := *nextSlot
		*nextSlot++
		slots[node.Var] = s
		return s, false
	}
	s := *nextSlot
	*nextSlot++
	return s, false
}

// idSeekLiteral recognizes a WHERE id(var) = <int|param> conjunct anchoring
// var to a single node id; nil when absent. Either argument order; only
// top-level AND conjuncts are inspected.
func idSeekLiteral(where ast.Expr, varName string) *ast.Literal {
	if where == nil {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	for _, c := range conjs {
		if l := idEqConjunct(c, varName); l != nil {
			return l
		}
	}
	return nil
}

// textSeek is a recognized substring-index anchor predicate.
type textSeek struct {
	field  string
	mode   ast.BinOp
	needle ast.Literal
}

// textMatchSeek recognizes a WHERE var.field {STARTS WITH|ENDS WITH|
// CONTAINS} <string|param> conjunct so the anchor scan can be served from a
// substring index. The conjunct is kept and re-checked.
func textMatchSeek(where ast.Expr, varName string) *textSeek {
	if where == nil {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	for _, c := range conjs {
		b, ok := c.(*ast.Binary)
		if !ok || (b.Op != ast.OpStartsWith && b.Op != ast.OpEndsWith && b.Op != ast.OpContains) {
			continue
		}
		p, ok := b.LHS.(*ast.Prop)
		if !ok || p.Var != varName {
			continue
		}
		l, ok := b.RHS.(*ast.Lit)
		if !ok {
			continue
		}
		switch l.Value.Kind {
		case ast.LitStr, ast.LitParam, ast.LitNamedParam:
			return &textSeek{field: p.Key, mode: b.Op, needle: l.Value}
		}
	}
	return nil
}

// idEqConjunct matches one conjunct as id(var) = lit / lit = id(var) with
// lit an Int / lifted Param / explicit NamedParam.
func idEqConjunct(c ast.Expr, varName string) *ast.Literal {
	b, ok := c.(*ast.Binary)
	if !ok || b.Op != ast.OpEq {
		return nil
	}
	litOf := func(e ast.Expr) *ast.Literal {
		l, ok := e.(*ast.Lit)
		if !ok {
			return nil
		}
		switch l.Value.Kind {
		case ast.LitInt, ast.LitParam, ast.LitNamedParam:
			v := l.Value
			return &v
		}
		return nil
	}
	if isIDOfVar(b.LHS, varName) {
		return litOf(b.RHS)
	}
	if isIDOfVar(b.RHS, varName) {
		return litOf(b.LHS)
	}
	return nil
}

// idSeekVar recognizes WHERE id(var) = <bound-var> (either order),
// returning the bound variable's slot; NoSlot when absent.
func idSeekVar(where ast.Expr, varName string, slots map[string]int, bound map[int]bool) int {
	if where == nil {
		return NoSlot
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	for _, c := range conjs {
		if s := idEqVarConjunct(c, varName, slots, bound); s != NoSlot {
			return s
		}
	}
	return NoSlot
}

// idEqVarConjunct matches id(var) = boundVar / boundVar = id(var),
// returning the bound variable's slot (NoSlot otherwise).
func idEqVarConjunct(c ast.Expr, varName string, slots map[string]int, bound map[int]bool) int {
	b, ok := c.(*ast.Binary)
	if !ok || b.Op != ast.OpEq {
		return NoSlot
	}
	boundSlot := func(e ast.Expr) int {
		v, ok := e.(*ast.Var)
		if !ok || v.Name == varName {
			return NoSlot
		}
		if s, ok := slots[v.Name]; ok && bound[s] {
			return s
		}
		return NoSlot
	}
	if isIDOfVar(b.LHS, varName) {
		return boundSlot(b.RHS)
	}
	if isIDOfVar(b.RHS, varName) {
		return boundSlot(b.LHS)
	}
	return NoSlot
}

// isIDOfVar matches the single-argument id() function applied to exactly
// varName.
func isIDOfVar(e ast.Expr, varName string) bool {
	f, ok := e.(*ast.Func)
	if !ok || f.Distinct || f.Star || len(f.Args) != 1 || !eqFold(f.Name, "id") {
		return false
	}
	v, ok := f.Args[0].(*ast.Var)
	return ok && v.Name == varName
}

// propEq is a var-property equality recognized as an indexed-seek anchor:
// key = val, val a seekable literal (Null excluded).
type propEq struct {
	key string
	val ast.Literal
}

// propEqConjuncts collects the top-level WHERE conjuncts of the form
// `varName.key = <literal|param>` (either operand order) -- the seekable
// equalities the inline `{key: val}` spelling would have produced, so the two
// spellings of one query plan alike. Only top-level AND conjuncts qualify: an
// equality nested under an OR is not a guaranteed filter and must not lift.
func propEqConjuncts(where ast.Expr, varName string) []propEq {
	if where == nil || varName == "" {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	var out []propEq
	for _, c := range conjs {
		b, ok := c.(*ast.Binary)
		if !ok || b.Op != ast.OpEq {
			continue
		}
		if k, v, ok := propEqSide(b.LHS, b.RHS, varName); ok {
			out = append(out, propEq{key: k, val: v})
		} else if k, v, ok := propEqSide(b.RHS, b.LHS, varName); ok {
			out = append(out, propEq{key: k, val: v})
		}
	}
	return out
}

// propEqSide matches propExpr as varName.key and litExpr as a seekable literal,
// returning (key, value). A Null literal is rejected (= null is never true, so
// it is not a seek). Params are accepted: they seek but abstain from costing.
func propEqSide(propExpr, litExpr ast.Expr, varName string) (string, ast.Literal, bool) {
	p, ok := propExpr.(*ast.Prop)
	if !ok || p.Var != varName {
		return "", ast.Literal{}, false
	}
	l, ok := litExpr.(*ast.Lit)
	if !ok {
		return "", ast.Literal{}, false
	}
	switch l.Value.Kind {
	case ast.LitInt, ast.LitFloat, ast.LitStr, ast.LitBool, ast.LitParam, ast.LitNamedParam:
		return p.Key, l.Value, true
	}
	return "", ast.Literal{}, false
}

// propSeekPick is the property a labelled node anchors on via the value index,
// chosen over both inline `{key: val}` props and top-level WHERE equalities on
// the node. A concrete value carries its exact posting length; a param abstains
// (no plan-time value) and is used only when nothing concrete seeks -- so a
// param never bakes a value into a shared cached plan.
type propSeekPick struct {
	key     string
	val     ast.Literal
	card    uint64 // exact posting length; meaningful only when !abstain
	abstain bool   // param value: seekable but uncosted
}

// bestPropSeek is the single source of truth for which property a fresh
// labelled node seeks on: the most selective one -- smallest exact posting
// length -- across inline props and WHERE-form equalities. rank, anchorCard,
// resolveAnchorNodes and scanSource all consult it, so the plan that is COSTED
// is always the plan that is BUILT (the two drifting apart is the bug 107
// names). A concrete prop always beats a param; among concretes, min posting
// wins; ok=false for an unlabelled node or one with no seekable prop.
func bestPropSeek(node *ast.NodePat, where ast.Expr, g graph.Graph) (propSeekPick, bool) {
	if len(node.Labels) == 0 {
		return propSeekPick{}, false
	}
	label := node.Labels[0]
	var best propSeekPick
	found := false
	consider := func(key string, val ast.Literal) {
		switch val.Kind {
		case ast.LitNull:
			return
		case ast.LitParam, ast.LitNamedParam:
			if !found { // a param seeks, but any concrete prop is preferred
				best = propSeekPick{key: key, val: val, abstain: true}
				found = true
			}
			return
		}
		c := uint64(setLen(g.NodesWithProperty(label, key, semantics.LitValue(val))))
		if !found || best.abstain || c < best.card {
			best = propSeekPick{key: key, val: val, card: c}
			found = true
		}
	}
	for i := range node.Props {
		consider(node.Props[i].Key, node.Props[i].Val)
	}
	for _, eq := range propEqConjuncts(where, node.Var) {
		consider(eq.key, eq.val)
	}
	return best, found
}

// scanSource picks a fresh node's default scan source: the most selective
// indexed property seek over its inline props and WHERE equalities (via
// bestPropSeek), else a label scan, else all nodes.
func scanSource(node *ast.NodePat, where ast.Expr, g graph.Graph) ScanSource {
	if ps, ok := bestPropSeek(node, where, g); ok {
		return ScanSource{Kind: ScanProperty, Label: node.Labels[0], Key: ps.key, Value: ps.val}
	}
	if len(node.Labels) > 0 {
		return ScanSource{Kind: ScanLabel, Label: node.Labels[0]}
	}
	return ScanSource{Kind: ScanAll}
}

// buildHop lowers one relationship hop to an Expand or VarExpand op.
func buildHop(rel *ast.RelPat, from, to, relSlot int, rebind bool, node *ast.NodePat) (BindOp, error) {
	op := BindOp{
		From:    from,
		To:      to,
		Rebind:  rebind,
		Dir:     mapDir(rel.Dir),
		Types:   rel.Types,
		RelSlot: relSlot,
		Labels:  node.Labels,
		Props:   node.Props,
	}
	if rel.Length == nil {
		op.Kind = OpExpand
		return op, nil
	}
	// GQL quantifier defaulting: an absent lower bound is 0 ({,3}, *),
	// per GRAMMAR.md -- unlike Cypher's *..3, whose min defaults to 1.
	vl := rel.Length
	var minHops uint64
	if vl.Min != nil {
		minHops = *vl.Min
	}
	if vl.Max != nil && minHops > *vl.Max {
		return BindOp{}, planErrf("quantifier bound {%d,%d} is empty", minHops, *vl.Max)
	}
	// Zero-length or unbounded resolves the distinct reachable set (a
	// dedup'd BFS), which has no per-path rel list, so a named rel
	// variable is not supported there.
	reach := minHops == 0 || vl.Max == nil
	if reach && rel.Var != "" {
		return BindOp{}, planErrf("a relationship variable is not supported on a zero-length or unbounded quantified pattern -- it resolves a reachable set, not paths")
	}
	op.Kind = OpVarExpand
	op.Min = minHops
	op.Max = vl.Max
	op.RelVar = rel.Var
	return op, nil
}

// extractVarlenHopPreds lifts all(r IN rels(e) WHERE pred) conjuncts from
// a stage WHERE onto the matching var-expand as a per-hop filter; consumed
// conjuncts drop from the WHERE. (Monotonic-order conjuncts lift later, in
// the segment-wide pushMonoPreds pass.)
func extractVarlenHopPreds(where *ast.Expr, ops []BindOp) error {
	if *where == nil {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(*where, &conjs)
	var kept []ast.Expr
	for _, c := range conjs {
		pushed, err := tryPushHopPred(c, ops)
		if err != nil {
			return err
		}
		if pushed {
			continue
		}
		kept = append(kept, c)
	}
	*where = rebuildAnd(kept)
	return nil
}

// tryPushHopPred lifts a single all(r IN rels(e) WHERE pred-over-r)
// conjunct onto the var-expand binding e. Any other shape returns false
// and is handled generally (never rejected here).
func tryPushHopPred(c ast.Expr, ops []BindOp) (bool, error) {
	lp, ok := c.(*ast.ListPred)
	if !ok {
		return false, nil
	}
	e := relsArg(lp.List)
	if e == "" {
		return false, nil
	}
	var target *BindOp
	for i := range ops {
		if ops[i].Kind == OpVarExpand && ops[i].RelVar == e {
			target = &ops[i]
			break
		}
	}
	if target == nil {
		return false, nil
	}
	if lp.Quant != ast.QuantAll || predRefsOnly(lp.Pred, lp.Var) != nil {
		return false, nil
	}
	if target.RelPred != nil {
		return false, nil
	}
	target.RelPred = &RelHopPred{Var: lp.Var, Pred: lp.Pred}
	return true, nil
}

// relsArg matches rels(<var>) returning the variable name; "" otherwise.
func relsArg(e ast.Expr) string {
	f, ok := e.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 1 || !eqFold(f.Name, "rels") {
		return ""
	}
	v, ok := f.Args[0].(*ast.Var)
	if !ok {
		return ""
	}
	return v.Name
}

// predRefsOnly validates that a per-hop predicate's free variables are
// only the iteration variable v (so it can be evaluated against one
// relationship).
func predRefsOnly(e ast.Expr, v string) error {
	if bad := freeVarsOutside(e, []string{v}); len(bad) > 0 {
		return planErrf("a per-hop predicate may only reference the relationship variable `%s` (found `%s`)", v, bad[0])
	}
	return nil
}

// SplitAnd walks a WHERE's top-level AND chain, appending each conjunct
// (exported: the executor's conjunct bucketing splits the same chains).
func SplitAnd(e ast.Expr, out *[]ast.Expr) {
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.OpAnd {
		SplitAnd(b.LHS, out)
		SplitAnd(b.RHS, out)
		return
	}
	*out = append(*out, e)
}

// rebuildAnd re-joins conjuncts left-associatively; nil for an empty list.
func rebuildAnd(conjs []ast.Expr) ast.Expr {
	if len(conjs) == 0 {
		return nil
	}
	acc := conjs[0]
	for _, c := range conjs[1:] {
		acc = &ast.Binary{Op: ast.OpAnd, LHS: acc, RHS: c}
	}
	return acc
}

// mapDir converts pattern syntax direction to a traversal direction.
func mapDir(d ast.Dir) graph.Direction {
	switch d {
	case ast.DirOut:
		return graph.Outgoing
	case ast.DirIn:
		return graph.Incoming
	}
	return graph.Both
}
