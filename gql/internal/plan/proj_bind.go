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
	// The explicit items carry the output-column names: an alias, else the
	// derived name. Two explicit items may not share a name -- the result
	// table cannot have two columns of the same name, and a later ORDER BY /
	// column reference to it would be ambiguous (openCypher and ISO GQL both
	// reject it). An explicit item also shadows a same-named `*` variable, so
	// the expanded projection stays a set of distinct names (this is how a
	// re-binding LET, desugared to `WITH *, expr AS x`, keeps a single x).
	explicit := make([]BoundReturn, 0, len(proj.Items))
	explicitNames := make(map[string]bool, len(proj.Items))
	for _, item := range proj.Items {
		isAgg := semantics.ExprHasAgg(item.Expr)
		name := item.Alias
		if name == "" {
			name = semantics.DerivedName(item.Expr)
		}
		if explicitNames[name] {
			return ProjPlan{}, bindErrf("duplicate output column name %q: a projection cannot return two columns with the same name", name)
		}
		explicitNames[name] = true
		explicit = append(explicit, BoundReturn{Expr: item.Expr, Name: name, IsAgg: isAgg})
	}
	// `*` expands to every in-scope variable ahead of the explicit items, in
	// introduction (slot) order so column order is stable -- minus any
	// variable an explicit item re-projects under the same name (shadowed).
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
			if explicitNames[v.name] {
				continue
			}
			returns = append(returns, BoundReturn{Expr: &ast.Var{Name: v.name}, Name: v.name})
		}
	}
	returns = append(returns, explicit...)
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
	case *ast.IsTruth:
		e, err := extractNestedAggs(n.Expr, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.IsTruth{Expr: e, Want: n.Want, Negated: n.Negated}, nil
	case *ast.IsTyped:
		e, err := extractNestedAggs(n.Expr, nCols, hidden, aggs)
		if err != nil {
			return nil, err
		}
		return &ast.IsTyped{Expr: e, Kind: n.Kind, Negated: n.Negated}, nil
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
