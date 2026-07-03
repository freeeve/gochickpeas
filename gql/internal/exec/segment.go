// Segment runner: seed the input rows to full width, run the stages in
// order, project, then apply the boundary's WHERE. The Rust streaming
// top-k / streaming-aggregate fast paths land with M18; results here are
// identical (they were pure materialization optimizations).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// runSegment runs one segment over its input rows.
func runSegment(ctx *eval.Ctx, seg *plan.Segment, inputs [][]value.Value) [][]value.Value {
	matched := make([][]value.Value, len(inputs))
	for i, inrow := range inputs {
		base := make([]value.Value, seg.RowWidth)
		copy(base, inrow)
		matched[i] = base
	}
	for _, st := range seg.Stages {
		switch s := st.(type) {
		case *plan.MatchStage:
			matched = runStage(ctx, s, seg.Slots, matched)
		case *plan.SpStage:
			matched = runSPStage(ctx, s, matched)
		case *plan.CallStage:
			matched = runCallStage(ctx, s, matched)
		case *plan.UnwindStage:
			matched = runUnwindStage(ctx, s, seg.Slots, matched)
		case *plan.CallSubqueryStage:
			matched = runCallSubqueryStage(ctx, s, matched)
		}
	}
	var out [][]value.Value
	if seg.Proj.Aggregated {
		out = aggregate(ctx, &seg.Proj, seg.Slots, matched)
	} else {
		out = project(ctx, &seg.Proj, seg.Slots, matched)
	}
	applyPostWhere(ctx, seg, &out)
	return out
}

// applyPostWhere applies a segment's projection-boundary WHERE (FILTER /
// RETURN...NEXT guard) to its output rows in place, by output column.
func applyPostWhere(ctx *eval.Ctx, seg *plan.Segment, out *[][]value.Value) {
	if seg.PostWhere == nil {
		return
	}
	scope := make(map[string]int, len(seg.Proj.Columns))
	for i, c := range seg.Proj.Columns {
		scope[c] = i
	}
	// An output column constant across every projected row (e.g. an
	// ungrouped collect broadcast) lets an IN over it probe a prebuilt set.
	rows := *out
	isConst := func(s int) bool { return slotConstant(s, rows) }
	var sample []value.Value
	if len(rows) > 0 {
		sample = rows[0]
	}
	w := hoistEval(ctx, compileEval(ctx, seg.PostWhere, scope), isConst, func(int) bool { return false }, sample, scope)
	kept := (*out)[:0]
	for _, r := range *out {
		if w.Eval(ctx, r, scope).IsTruthy() {
			kept = append(kept, r)
		}
	}
	*out = kept
}
