// Row-source stages: FOR (unwind a list into rows) and CALL { }
// correlated subqueries (a lateral join / flat-map over a sub-plan).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// runUnwindStage: each input row evaluates the list; a list emits one row
// per element, null emits none (a left-anti filter for those rows), any
// other scalar emits a single row bound to it.
func runUnwindStage(ctx *eval.Ctx, us *plan.UnwindStage, slots map[string]int, rows [][]value.Value) [][]value.Value {
	list := compileEval(ctx, us.List, slots)
	out := make([][]value.Value, 0, len(rows))
	for _, row := range rows {
		v := list.Eval(ctx, row, slots)
		if items, ok := v.AsList(); ok {
			for _, item := range items {
				r := make([]value.Value, len(row))
				copy(r, row)
				r[us.OutSlot] = item
				out = append(out, r)
			}
			continue
		}
		if v.IsNull() {
			continue
		}
		r := make([]value.Value, len(row))
		copy(r, row)
		r[us.OutSlot] = v
		out = append(out, r)
	}
	return out
}

// runCallSubqueryStage: for each outer row, seed the sub-plan with the
// imported values and emit one merged row per sub-output row (inner-join
// semantics; an aggregating subquery always returns one row, so its outer
// row survives). An uncorrelated subquery evaluates once and cross-joins.
func runCallSubqueryStage(ctx *eval.Ctx, cs *plan.CallSubqueryStage, rows [][]value.Value) [][]value.Value {
	var out [][]value.Value
	emit := func(row []value.Value, sub [][]value.Value) {
		for _, s := range sub {
			r := make([]value.Value, len(row))
			copy(r, row)
			for i, slot := range cs.OutSlots {
				r[slot] = s[i]
			}
			out = append(out, r)
		}
	}
	if len(cs.ImportSlots) == 0 {
		subRows := runSubplan(ctx, cs.Sub, nil)
		for _, row := range rows {
			emit(row, subRows)
		}
		return out
	}
	for _, row := range rows {
		seed := make([]value.Value, len(cs.ImportSlots))
		for i, s := range cs.ImportSlots {
			seed[i] = row[s]
		}
		emit(row, runSubplan(ctx, cs.Sub, seed))
	}
	return out
}

// runSubplan runs a sub-plan over one seed row, combining its UNION
// branches exactly like the top-level Execute.
func runSubplan(ctx *eval.Ctx, sub *plan.Plan, seed []value.Value) [][]value.Value {
	acc := runBranchSeeded(ctx, sub.Branches[0], seed)
	for i, op := range sub.Union {
		combineUnion(&acc, runBranchSeeded(ctx, sub.Branches[i+1], seed), op)
	}
	return acc
}

// runBranchSeeded runs a branch's segment pipeline from one seed row.
func runBranchSeeded(ctx *eval.Ctx, segments []*plan.Segment, seed []value.Value) [][]value.Value {
	rows := [][]value.Value{seed}
	for _, seg := range segments {
		rows = runSegment(ctx, seg, rows)
	}
	return rows
}
