// Aggregating-projection helpers: substituting grouping-key subexpressions
// with references to their output columns, and compiling a top-level
// aggregate call into an AggCol. Split from proj_bind.go, which holds the
// projection binding and nested-aggregate extraction.
package plan

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// groupCol is one grouping item of an aggregating projection: its output
// column index, name, and source expression.
type groupCol struct {
	idx  int
	name string
	expr ast.Expr
}

// alphaLocal renames the binder local *v to a fresh name when it collides
// with a grouping column's output name, rewriting the bodies in place --
// substituting a column reference into the body can then never be captured
// by the local. avoid carries sibling locals of the same binder so the
// fresh name cannot collide with them either; nil bodies are skipped.
func alphaLocal(v *string, groups []groupCol, avoid []string, bodies ...*ast.Expr) {
	clash := false
	for _, g := range groups {
		if g.name == *v {
			clash = true
			break
		}
	}
	if !clash {
		return
	}
	used := map[string]bool{}
	for _, a := range avoid {
		used[a] = true
	}
	for _, b := range bodies {
		if *b != nil {
			collectAllVars(*b, used)
		}
	}
	for _, g := range groups {
		used[g.name] = true
		collectAllVars(g.expr, used)
	}
	var fresh string
	for i := 1; ; i++ {
		fresh = fmt.Sprintf("%s__%d", *v, i)
		if !used[fresh] {
			break
		}
	}
	for _, b := range bodies {
		if *b != nil {
			*b = renameFree(*b, *v, fresh)
		}
	}
	*v = fresh
}

// shadowFiltered returns groups minus the ones a binder's locals make
// unusable in its sub-scope: a group whose key expression mentions a local
// (the body reference means the local, not the key) or whose output name
// IS a local (defense in depth -- alphaLocal renames such locals away
// before this filter runs on the wired paths). The common case -- no
// collision -- returns groups unchanged without allocating.
func shadowFiltered(groups []groupCol, locals ...string) []groupCol {
	drop := func(g groupCol) bool {
		for _, l := range locals {
			if l == "" {
				continue
			}
			if g.name == l || MentionsVar(g.expr, l) {
				return true
			}
		}
		return false
	}
	for i, g := range groups {
		if drop(g) {
			kept := make([]groupCol, 0, len(groups)-1)
			kept = append(kept, groups[:i]...)
			for _, h := range groups[i+1:] {
				if !drop(h) {
					kept = append(kept, h)
				}
			}
			return kept
		}
	}
	return groups
}

// substGroupKeys re-points wrapper subexpressions at the grouping columns:
// a subexpression structurally equal to a grouping item's expression
// becomes a reference to its output column. Aggregates were already pulled
// out by extractNestedAggs, so no aggregate call remains in the wrapper.
// Comprehension/quantifier/reduce bodies bind their own variables but
// still evaluate post-projection, so the walk descends into them: a local
// that collides with a group's output name is first alpha-renamed to a
// fresh name (substituting the column reference could otherwise be
// captured), and a group is dropped for a sub-scope when the binder
// shadows a variable its key expression mentions -- that body reference
// means the local, not the key. Correlated subquery interiors
// (EXISTS/COUNT/pattern comprehensions) remain untouched: their filters
// evaluate against the subquery's own pattern scope, and a composite key
// appearing only there stays a bind error.
func substGroupKeys(e ast.Expr, groups []groupCol) ast.Expr {
	for _, g := range groups {
		if exprEqual(g.expr, e) {
			return &ast.Var{Name: g.name}
		}
	}
	switch n := e.(type) {
	// `var.key` where `var` is a grouping key projected under a different
	// name: re-point the property access at the output column
	// (`RETURN n AS m, n.x + count(*)`).
	case *ast.Prop:
		for _, g := range groups {
			if v, ok := g.expr.(*ast.Var); ok && v.Name == n.Var && g.name != n.Var {
				return &ast.PropOf{Base: &ast.Var{Name: g.name}, Key: n.Key}
			}
		}
	// `var:Label` where `var` is a grouping key projected under a different
	// name: the label test references its variable by name, so it needs the
	// same re-pointing as a property access (`RETURN c AS comp, count(*) +
	// (CASE WHEN c:Company THEN 1 ELSE 0 END) AS x`).
	case *ast.HasLabelExpr:
		for _, g := range groups {
			if v, ok := g.expr.(*ast.Var); ok && v.Name == n.Var && g.name != n.Var {
				return &ast.HasLabelExpr{Var: g.name, Expr: n.Expr}
			}
		}
	case *ast.Unary:
		n.Expr = substGroupKeys(n.Expr, groups)
	case *ast.IsNull:
		n.Expr = substGroupKeys(n.Expr, groups)
	case *ast.IsTruth:
		n.Expr = substGroupKeys(n.Expr, groups)
	case *ast.IsTyped:
		n.Expr = substGroupKeys(n.Expr, groups)
	case *ast.PropOf:
		n.Base = substGroupKeys(n.Base, groups)
	case *ast.Binary:
		n.LHS = substGroupKeys(n.LHS, groups)
		n.RHS = substGroupKeys(n.RHS, groups)
	case *ast.In:
		n.Expr = substGroupKeys(n.Expr, groups)
		n.List = substGroupKeys(n.List, groups)
	case *ast.Index:
		n.Base = substGroupKeys(n.Base, groups)
		n.Idx = substGroupKeys(n.Idx, groups)
	case *ast.Func:
		for i := range n.Args {
			n.Args[i] = substGroupKeys(n.Args[i], groups)
		}
	case *ast.ListExpr:
		for i := range n.Elems {
			n.Elems[i] = substGroupKeys(n.Elems[i], groups)
		}
	case *ast.Case:
		if n.Operand != nil {
			n.Operand = substGroupKeys(n.Operand, groups)
		}
		for i := range n.Whens {
			n.Whens[i].Cond = substGroupKeys(n.Whens[i].Cond, groups)
			n.Whens[i].Result = substGroupKeys(n.Whens[i].Result, groups)
		}
		if n.Else != nil {
			n.Else = substGroupKeys(n.Else, groups)
		}
	case *ast.Slice:
		n.Base = substGroupKeys(n.Base, groups)
		if n.From != nil {
			n.From = substGroupKeys(n.From, groups)
		}
		if n.To != nil {
			n.To = substGroupKeys(n.To, groups)
		}
	// Sources evaluate in the outer scope; the bodies descend after any
	// local colliding with an output name is alpha-renamed away, with the
	// remaining shadowed keys filtered out of the group set (see above).
	case *ast.ListPred:
		n.List = substGroupKeys(n.List, groups)
		alphaLocal(&n.Var, groups, nil, &n.Pred)
		n.Pred = substGroupKeys(n.Pred, shadowFiltered(groups, n.Var))
	case *ast.ListComp:
		n.List = substGroupKeys(n.List, groups)
		alphaLocal(&n.Var, groups, nil, &n.Filter, &n.Map)
		inner := shadowFiltered(groups, n.Var)
		if n.Filter != nil {
			n.Filter = substGroupKeys(n.Filter, inner)
		}
		if n.Map != nil {
			n.Map = substGroupKeys(n.Map, inner)
		}
	case *ast.Reduce:
		n.Init = substGroupKeys(n.Init, groups)
		n.List = substGroupKeys(n.List, groups)
		alphaLocal(&n.Acc, groups, []string{n.Var}, &n.Body)
		alphaLocal(&n.Var, groups, []string{n.Acc}, &n.Body)
		n.Body = substGroupKeys(n.Body, shadowFiltered(groups, n.Acc, n.Var))
	case *ast.MapLit:
		for i := range n.Fields {
			n.Fields[i].Val = substGroupKeys(n.Fields[i].Val, groups)
		}
	case *ast.MapProj:
		// The projected variable is held by name, so it re-points like a
		// property access or label test when its key is renamed.
		for _, g := range groups {
			if v, ok := g.expr.(*ast.Var); ok && v.Name == n.Var && g.name != n.Var {
				n.Var = g.name
				break
			}
		}
		for i := range n.Entries {
			if n.Entries[i].Kind == ast.MapProjField {
				n.Entries[i].Expr = substGroupKeys(n.Entries[i].Expr, groups)
			}
		}
	}
	return e
}

// extractAgg compiles a top-level aggregate projection expression.
func extractAgg(expr ast.Expr, outIdx int) (AggCol, error) {
	f, ok := expr.(*ast.Func)
	if !ok {
		return AggCol{}, planErrf("an aggregate must be a top-level projection expression")
	}
	var kind AggKind
	switch lower(f.Name) {
	case "count":
		kind = AggCount
	case "sum":
		kind = AggSum
	case "avg":
		kind = AggAvg
	case "min":
		kind = AggMin
	case "max":
		kind = AggMax
	case "collect", "collect_list":
		kind = AggCollect
	case "stddev_samp":
		kind = AggStddevSamp
	case "stddev_pop":
		kind = AggStddevPop
	case "percentile_cont", "percentilecont":
		kind = AggPercentileCont
	case "percentile_disc", "percentiledisc":
		kind = AggPercentileDisc
	default:
		return AggCol{}, planErrf("aggregate function `%s` is not supported (Tier 1)", f.Name)
	}
	if kind == AggPercentileCont || kind == AggPercentileDisc {
		if f.Star || len(f.Args) != 2 {
			return AggCol{}, planErrf("`%s(...)` takes exactly two arguments (a value and a percentile)", f.Name)
		}
		// The percentile is per-query, not per-row: a constant literal
		// (number or parameter) keeps the accumulator single-valued.
		if _, ok := f.Args[1].(*ast.Lit); !ok {
			return AggCol{}, planErrf("`%s(...)` requires a constant percentile", f.Name)
		}
		return AggCol{Kind: kind, Arg: f.Args[0], Arg2: f.Args[1], Distinct: f.Distinct, OutIdx: outIdx}, nil
	}
	var arg ast.Expr
	switch {
	case f.Star:
		if kind != AggCount {
			return AggCol{}, planErrf("`%s(*)` is not valid", f.Name)
		}
	case len(f.Args) == 1:
		arg = f.Args[0]
	default:
		return AggCol{}, planErrf("`%s(...)` takes exactly one argument (Tier 1)", f.Name)
	}
	return AggCol{Kind: kind, Arg: arg, Distinct: f.Distinct, OutIdx: outIdx}, nil
}
