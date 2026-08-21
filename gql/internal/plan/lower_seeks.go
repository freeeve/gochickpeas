// Anchor-seek recognizers (split from lower.go): the WHERE/inline-prop
// conjunct forms that let a fresh node anchor on an index seek instead
// of a label scan -- id equality (literal, param, and bound-variable),
// substring text match, property equality, and property IN lists.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

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
// param never bakes a value into a shared cached plan. inVals non-nil marks a
// multi-value IN seek (card = summed posting lengths).
type propSeekPick struct {
	key     string
	val     ast.Literal
	inVals  []ast.Literal
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
		c := seekCard(g, label, key, semantics.LitValue(val))
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
	for _, in := range propInConjuncts(where, node.Var) {
		var c uint64
		for _, v := range in.vals {
			c += seekCard(g, label, in.key, semantics.LitValue(v))
		}
		if !found || best.abstain || c < best.card {
			best = propSeekPick{key: in.key, inVals: in.vals, card: c}
			found = true
		}
	}
	return best, found
}

// DisablePropInSeek pins result identity in tests: the label-scan +
// filter evaluation must produce exactly the rows the IN seek does, in
// the same order.
var DisablePropInSeek bool

// propIn is one `<var>.<key> IN [literals]` conjunct usable as a seek.
type propIn struct {
	key  string
	vals []ast.Literal
}

// propInConjuncts collects WHERE conjuncts of the form
// `<var>.<key> IN [<literal>, ...]` whose every element is a string,
// boolean, or numeric literal. IN compares int against float numerically
// (30 IN [30.0] is true) while a property seek matches the stored value
// exactly, so the seek probes each numeric element's twin alongside it
// (seekNodes) -- without that a candidate the seek never yields is a row
// silently lost, the one failure keep-and-re-check cannot catch.
// (The sibling engine shipped the numeric form without twin probes and a
// mixed int/float test caught it; rustychickpeas c07ce7f.)
func propInConjuncts(where ast.Expr, varName string) []propIn {
	if DisablePropInSeek || where == nil || varName == "" {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	var out []propIn
	for _, c := range conjs {
		in, ok := c.(*ast.In)
		if !ok {
			continue
		}
		p, ok := in.Expr.(*ast.Prop)
		if !ok || p.Var != varName {
			continue
		}
		list, ok := in.List.(*ast.ListExpr)
		if !ok || len(list.Elems) == 0 {
			continue
		}
		vals := make([]ast.Literal, 0, len(list.Elems))
		qualified := true
		for _, el := range list.Elems {
			lit, isLit := el.(*ast.Lit)
			if !isLit {
				qualified = false
				break
			}
			switch lit.Value.Kind {
			case ast.LitStr, ast.LitBool, ast.LitInt, ast.LitFloat:
			default:
				qualified = false
			}
			if !qualified {
				break
			}
			vals = append(vals, lit.Value)
		}
		if qualified {
			out = append(out, propIn{key: p.Key, vals: vals})
		}
	}
	return out
}
