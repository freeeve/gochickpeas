// Core lowering primitives (port of the Rust plan/lower.rs): slot
// assignment, anchor seeks (id/text-match recognition), scan-source
// selection, hop building, var-length hop-predicate extraction, projection
// binding, and the shared AND-chain helpers.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
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
	splitAndRef(where, &conjs)
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
	splitAndRef(where, &conjs)
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
	splitAndRef(where, &conjs)
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

// scanSource picks a fresh node's default scan source from its labels and
// inline props: an indexed property seek, a label scan, or all nodes.
func scanSource(node *ast.NodePat) ScanSource {
	var anchor *ast.PropEntry
	for i := range node.Props {
		if node.Props[i].Val.Kind != ast.LitNull {
			anchor = &node.Props[i]
			break
		}
	}
	if len(node.Labels) > 0 && anchor != nil {
		return ScanSource{Kind: ScanProperty, Label: node.Labels[0], Key: anchor.Key, Value: anchor.Val}
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
// a stage WHERE onto the matching var-expand as a per-hop filter, and
// monotonic-order conjuncts as MonoHopSpecs; consumed conjuncts drop from
// the WHERE.
func extractVarlenHopPreds(where *ast.Expr, ops []BindOp) error {
	if *where == nil {
		return nil
	}
	var conjs []ast.Expr
	splitAndRef(*where, &conjs)
	var kept []ast.Expr
	for _, c := range conjs {
		pushed, err := tryPushHopPred(c, ops)
		if err != nil {
			return err
		}
		if pushed {
			continue
		}
		if tryPushMonoPred(c, ops) {
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

// predRefsOnly validates that a per-hop predicate references only the
// iteration variable v (so it can be evaluated against one relationship).
func predRefsOnly(e ast.Expr, v string) error {
	badVar := func() error {
		return planErrf("a per-hop predicate may only reference the relationship variable `%s`", v)
	}
	switch n := e.(type) {
	case *ast.Lit:
		return nil
	case *ast.Var:
		if n.Name != v {
			return badVar()
		}
		return nil
	case *ast.Prop:
		if n.Var != v {
			return badVar()
		}
		return nil
	case *ast.Unary:
		return predRefsOnly(n.Expr, v)
	case *ast.IsNull:
		return predRefsOnly(n.Expr, v)
	case *ast.Binary:
		if err := predRefsOnly(n.LHS, v); err != nil {
			return err
		}
		return predRefsOnly(n.RHS, v)
	case *ast.In:
		if err := predRefsOnly(n.Expr, v); err != nil {
			return err
		}
		return predRefsOnly(n.List, v)
	case *ast.ListExpr:
		for _, x := range n.Elems {
			if err := predRefsOnly(x, v); err != nil {
				return err
			}
		}
		return nil
	case *ast.Func:
		for _, x := range n.Args {
			if err := predRefsOnly(x, v); err != nil {
				return err
			}
		}
		return nil
	}
	return planErrf("unsupported expression in a per-hop relationship predicate")
}

// splitAndRef walks a WHERE's top-level AND chain, appending each conjunct.
func splitAndRef(e ast.Expr, out *[]ast.Expr) {
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.OpAnd {
		splitAndRef(b.LHS, out)
		splitAndRef(b.RHS, out)
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
