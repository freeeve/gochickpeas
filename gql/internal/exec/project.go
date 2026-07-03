// Non-aggregated projection: evaluate the output columns, apply DISTINCT
// (before ORDER BY/LIMIT, as the standard requires), sort, and paginate.
// The Rust bounded top-k heap is a materialization optimization with
// byte-identical results; the stable sort + paginate here is the reference
// semantics (heap lands with the M18 streaming work if benchmarks want it).
package exec

import (
	"maps"
	"sort"

	"github.com/freeeve/gochickpeas/gql/internal/ast"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// project evaluates a non-aggregated projection over the matched rows.
func project(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, matched [][]value.Value) [][]value.Value {
	returns := make([]RowEval, len(proj.Returns))
	for i, r := range proj.Returns {
		returns[i] = compileEval(ctx, r.Expr, slots)
	}
	projRow := func(m []value.Value) []value.Value {
		out := make([]value.Value, len(returns))
		for i, c := range returns {
			out[i] = c.Eval(ctx, m, slots)
		}
		return out
	}

	if !proj.Distinct && len(proj.OrderBy) == 0 {
		out := make([][]value.Value, len(matched))
		for i, m := range matched {
			out[i] = projRow(m)
		}
		return paginate(out, proj.Skip, proj.Limit)
	}

	type pair struct {
		m, o []value.Value
	}
	pairs := make([]pair, len(matched))
	for i, m := range matched {
		pairs[i] = pair{m, projRow(m)}
	}

	if proj.Distinct {
		seen := make(map[string]struct{}, len(pairs))
		kept := pairs[:0]
		var key []byte
		for _, p := range pairs {
			key = key[:0]
			for _, v := range p.o {
				key = value.AppendKey(key, v)
			}
			if _, dup := seen[string(key)]; !dup {
				seen[string(key)] = struct{}{}
				kept = append(kept, p)
			}
		}
		pairs = kept
	}

	if len(proj.OrderBy) > 0 {
		sort.SliceStable(pairs, func(a, b int) bool {
			return cmpOrder(ctx, proj, slots, pairs[a].m, pairs[a].o, pairs[b].m, pairs[b].o) < 0
		})
	}

	out := make([][]value.Value, len(pairs))
	for i, p := range pairs {
		out[i] = p.o
	}
	return paginate(out, proj.Skip, proj.Limit)
}

// cmpOrder compares two (matched, projected) row pairs per the ORDER BY
// items.
func cmpOrder(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, am, ao, bm, bo []value.Value) int {
	for i := range proj.OrderBy {
		s := &proj.OrderBy[i]
		ka := orderKey(ctx, s, am, ao, proj, slots)
		kb := orderKey(ctx, s, bm, bo, proj, slots)
		ord := value.OrderCmp(ka, kb)
		if s.Desc {
			ord = -ord
		}
		if ord != 0 {
			return ord
		}
	}
	return 0
}

// orderKey resolves one ORDER BY key: a key that is a whole projected
// column (its alias or exact expression) reads the projected value
// directly; otherwise it evaluates over the incoming row extended with the
// projected columns, so it can combine projection aliases (which shadow
// same-named incoming variables) with incoming variables.
func orderKey(ctx *eval.Ctx, s *ast.SortItem, matched, out []value.Value, proj *plan.ProjPlan, slots map[string]int) value.Value {
	if idx := plan.OrderColIndex(s.Expr, proj.Columns, proj.Returns); idx >= 0 {
		return out[idx]
	}
	row := make([]value.Value, 0, len(matched)+len(out))
	row = append(row, matched...)
	row = append(row, out...)
	scope := make(map[string]int, len(slots)+len(proj.Columns))
	maps.Copy(scope, slots)
	for i, c := range proj.Columns {
		scope[c] = len(matched) + i
	}
	return eval.Eval(ctx, s.Expr, row, scope)
}

// paginate applies OFFSET/SKIP then LIMIT.
func paginate(v [][]value.Value, skip, limit *uint64) [][]value.Value {
	if skip != nil {
		s := int(*skip)
		if s >= len(v) {
			return nil
		}
		v = v[s:]
	}
	if limit != nil && uint64(len(v)) > *limit {
		v = v[:*limit]
	}
	return v
}
