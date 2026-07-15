// Projection binding (split from lower.go for the file-size norm):
// compile a projection body against the segment scope, resolve ORDER BY
// keys, and hoist nested aggregates into hidden accumulator slots.
package plan

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// bindProjection compiles a projection body against the segment scope.
func bindProjection(proj ast.Projection, scope map[string]int) (ProjPlan, error) {
	var returns []BoundReturn
	// `*` expands to every in-scope variable ahead of the explicit items,
	// in introduction (slot) order so column order is stable.
	if proj.Star {
		type nv struct {
			name string
			slot int
		}
		vars := make([]nv, 0, len(scope))
		for k, s := range scope {
			vars = append(vars, nv{k, s})
		}
		sortSlice(vars, func(a, b nv) bool { return a.slot < b.slot })
		if len(vars) == 0 && len(proj.Items) == 0 {
			return ProjPlan{}, bindErrf("RETURN */WITH * requires at least one variable in scope")
		}
		for _, v := range vars {
			returns = append(returns, BoundReturn{Expr: &ast.Var{Name: v.name}, Name: v.name})
		}
	}
	for _, item := range proj.Items {
		isAgg := semantics.ExprHasAgg(item.Expr)
		name := item.Alias
		if name == "" {
			name = semantics.DerivedName(item.Expr)
		}
		returns = append(returns, BoundReturn{Expr: item.Expr, Name: name, IsAgg: isAgg})
	}
	aggregated := false
	for i := range returns {
		aggregated = aggregated || returns[i].IsAgg
	}
	if proj.Distinct && aggregated {
		return ProjPlan{}, bindErrf("DISTINCT with aggregates is not supported")
	}
	for i := range returns {
		if err := semantics.CheckRefsSkippingAgg(returns[i].Expr, scope); err != nil {
			return ProjPlan{}, err
		}
	}

	columns := make([]string, len(returns))
	for i := range returns {
		columns[i] = returns[i].Name
	}
	// ORDER BY resolves against the output columns -- by alias or by a key
	// expression equal to a projected one. A plain projection additionally
	// keeps the incoming variables in scope; after aggregation or DISTINCT
	// only the output columns survive -- a key over a discarded variable
	// would be ambiguous per surviving row (both ISO GQL and openCypher
	// reject it), and the executor would silently evaluate it against the
	// first-encountered duplicate's bindings.
	orderScope := map[string]int{}
	if !aggregated && !proj.Distinct {
		for k, v := range scope {
			orderScope[k] = v
		}
	}
	for _, c := range columns {
		if _, ok := orderScope[c]; !ok {
			orderScope[c] = 0
		}
	}
	for _, s := range proj.OrderBy {
		if OrderColIndex(s.Expr, columns, returns) >= 0 {
			continue
		}
		if err := semantics.CheckRefs(s.Expr, orderScope); err != nil {
			if aggregated {
				return ProjPlan{}, bindErrf("ORDER BY must reference a projection column when aggregating")
			}
			if proj.Distinct {
				return ProjPlan{}, bindErrf("ORDER BY under DISTINCT must reference a projection column")
			}
			return ProjPlan{}, err
		}
	}

	nCols := len(returns)
	var groupIdx []int
	var aggs []AggCol
	var post []PostProj
	nHidden := 0
	// Grouping items (the non-aggregate projections, which may be listed
	// after a wrapper that uses them) are legal references inside a
	// nested-aggregate wrapper -- standard grouping semantics, e.g. the
	// carried alias in `RETURN xs, xs + collect(q) AS ys` (the Rust
	// engine's tasks/150).
	var groupCols []groupCol
	for i := range returns {
		if !returns[i].IsAgg {
			groupCols = append(groupCols, groupCol{idx: i, name: returns[i].Name, expr: returns[i].Expr})
		}
	}
	for i := range returns {
		r := &returns[i]
		if !r.IsAgg {
			groupIdx = append(groupIdx, i)
			continue
		}
		if isPureAgg(r.Expr) {
			a, err := extractAgg(r.Expr, i)
			if err != nil {
				return ProjPlan{}, err
			}
			aggs = append(aggs, a)
			continue
		}
		// A nested aggregate: hoist each aggregate into a hidden slot and
		// project the rewritten wrapper after grouping. The wrapper must
		// reduce to a scalar over those hidden slots and the grouping keys
		// -- any other variable reference is rejected.
		rewritten, err := extractNestedAggs(r.Expr, nCols, &nHidden, &aggs)
		if err != nil {
			return ProjPlan{}, err
		}
		rewritten = substGroupKeys(rewritten, groupCols)
		synth := make(map[string]int, nHidden+len(groupCols))
		for k := range nHidden {
			synth[fmt.Sprintf("__agg%d", k)] = nCols + k
		}
		for _, g := range groupCols {
			if _, ok := synth[g.name]; !ok {
				synth[g.name] = g.idx
			}
		}
		if err := semantics.CheckRefs(rewritten, synth); err != nil {
			return ProjPlan{}, err
		}
		post = append(post, PostProj{Col: i, Expr: rewritten})
	}

	return ProjPlan{
		Returns:    returns,
		Distinct:   proj.Distinct,
		Aggregated: aggregated,
		GroupIdx:   groupIdx,
		Aggs:       aggs,
		Post:       post,
		NHidden:    nHidden,
		OrderBy:    proj.OrderBy,
		Skip:       proj.Skip,
		Limit:      proj.Limit,
		Columns:    columns,
	}, nil
}

// OrderColIndex resolves an ORDER BY key to an output column as a whole: a
// bare variable naming a projected column, or a key expression structurally
// equal to a projected one. -1 when the key is a composite the executor
// evaluates against the projected row instead.
func OrderColIndex(expr ast.Expr, columns []string, returns []BoundReturn) int {
	if v, ok := expr.(*ast.Var); ok {
		for i, c := range columns {
			if c == v.Name {
				return i
			}
		}
	}
	for i := range returns {
		if exprEqual(returns[i].Expr, expr) {
			return i
		}
	}
	return -1
}

// isPureAgg reports a top-level pure aggregate projection (the aggregate
// is the whole expression, so it owns its column).
func isPureAgg(e ast.Expr) bool {
	f, ok := e.(*ast.Func)
	return ok && semantics.IsAggName(f.Name)
}

// extractNestedAggs rewrites a nested-aggregate projection: each aggregate
// sub-expression is pulled into a hidden slot (recorded in aggs) and
// replaced by a __agg{k} reference, yielding an aggregate-free wrapper
// evaluated once per finalized group. Recursion descends only through
// scalar operators/functions/CASE/list constructs.
func extractNestedAggs(expr ast.Expr, nCols int, hidden *int, aggs *[]AggCol) (ast.Expr, error) {
	if isPureAgg(expr) {
		k := *hidden
		a, err := extractAgg(expr, nCols+k)
		if err != nil {
			return nil, err
		}
		*aggs = append(*aggs, a)
		*hidden++
		return &ast.Var{Name: fmt.Sprintf("__agg%d", k)}, nil
	}
	if !semantics.ExprHasAgg(expr) {
		return expr, nil
	}
	switch n := expr.(type) {
	case *ast.Unary:
		e, err := extractNestedAggs(n.Expr, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: n.Op, Expr: e}, nil
	case *ast.Binary:
		l, err := extractNestedAggs(n.LHS, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		r, err := extractNestedAggs(n.RHS, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.Binary{Op: n.Op, LHS: l, RHS: r}, nil
	case *ast.Func:
		if n.Star {
			return expr, nil
		}
		mapped := make([]ast.Expr, len(n.Args))
		for i, a := range n.Args {
			m, err := extractNestedAggs(a, nCols, hidden, aggs)
			if err != nil {
				return nil, err
			}
			mapped[i] = m
		}
		return &ast.Func{Name: n.Name, Distinct: n.Distinct, Args: mapped}, nil
	case *ast.ListExpr:
		mapped := make([]ast.Expr, len(n.Elems))
		for i, a := range n.Elems {
			m, err := extractNestedAggs(a, nCols, hidden, aggs)
			if err != nil {
				return nil, err
			}
			mapped[i] = m
		}
		return &ast.ListExpr{Elems: mapped}, nil
	case *ast.In:
		e, err := extractNestedAggs(n.Expr, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		l, err := extractNestedAggs(n.List, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.In{Expr: e, List: l}, nil
	case *ast.IsNull:
		e, err := extractNestedAggs(n.Expr, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.IsNull{Expr: e, Negated: n.Negated}, nil
	case *ast.Reduce:
		init, err := extractNestedAggs(n.Init, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		list, err := extractNestedAggs(n.List, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		body, err := extractNestedAggs(n.Body, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.Reduce{Acc: n.Acc, Init: init, Var: n.Var, List: list, Body: body}, nil
	case *ast.Index:
		b, err := extractNestedAggs(n.Base, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		i, err := extractNestedAggs(n.Idx, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.Index{Base: b, Idx: i}, nil
	case *ast.Slice:
		b, err := extractNestedAggs(n.Base, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		out := &ast.Slice{Base: b}
		if n.From != nil {
			if out.From, err = extractNestedAggs(n.From, nCols, hidden, aggs); err != nil {
				return nil, err
			}
		}
		if n.To != nil {
			if out.To, err = extractNestedAggs(n.To, nCols, hidden, aggs); err != nil {
				return nil, err
			}
		}
		return out, nil
	case *ast.PropOf:
		b, err := extractNestedAggs(n.Base, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.PropOf{Base: b, Key: n.Key}, nil
	case *ast.Case:
		out := &ast.Case{}
		var err error
		if n.Operand != nil {
			if out.Operand, err = extractNestedAggs(n.Operand, nCols, hidden, aggs); err != nil {
				return nil, err
			}
		}
		out.Whens = make([]ast.CaseWhen, len(n.Whens))
		for i, w := range n.Whens {
			c, err := extractNestedAggs(w.Cond, nCols, hidden, aggs)
			if err != nil {
				return nil, err
			}
			r, err := extractNestedAggs(w.Result, nCols, hidden, aggs)
			if err != nil {
				return nil, err
			}
			out.Whens[i] = ast.CaseWhen{Cond: c, Result: r}
		}
		if n.Else != nil {
			if out.Else, err = extractNestedAggs(n.Else, nCols, hidden, aggs); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, planErrf("an aggregate here is not supported -- it must sit inside scalar arithmetic, a function call, CASE, or a list (Tier 1)")
}

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
	default:
		return AggCol{}, planErrf("aggregate function `%s` is not supported (Tier 1)", f.Name)
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
