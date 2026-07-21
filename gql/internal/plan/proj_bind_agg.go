// Aggregating-projection helpers: substituting grouping-key subexpressions
// with references to their output columns, and compiling a top-level
// aggregate call into an AggCol. Split from proj_bind.go, which holds the
// projection binding and nested-aggregate extraction.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// groupCol is one grouping item of an aggregating projection: its output
// column index, name, and source expression.
type groupCol struct {
	idx  int
	name string
	expr ast.Expr
}

// substGroupKeys re-points wrapper subexpressions at the grouping columns:
// a subexpression structurally equal to a grouping item's expression
// becomes a reference to its output column. Aggregates were already pulled
// out by extractNestedAggs, so no aggregate call remains in the wrapper.
// Interiors that bind their own variables (comprehension/quantifier/reduce
// bodies, correlated subquery filters) are left untouched: a bare-variable
// key still resolves there by name through the post-projection scope,
// while a composite key appearing only there stays a bind error rather
// than risking capture by the local binding.
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
	// Sources evaluate in the outer scope; the bodies bind their own
	// variables (see above).
	case *ast.ListPred:
		n.List = substGroupKeys(n.List, groups)
	case *ast.ListComp:
		n.List = substGroupKeys(n.List, groups)
	case *ast.Reduce:
		n.Init = substGroupKeys(n.Init, groups)
		n.List = substGroupKeys(n.List, groups)
	case *ast.MapLit:
		for i := range n.Fields {
			n.Fields[i].Val = substGroupKeys(n.Fields[i].Val, groups)
		}
	case *ast.MapProj:
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
