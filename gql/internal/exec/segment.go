// Segment runner: seed the input rows to full width, run the stages in
// order, project, then apply the boundary's WHERE. The Rust streaming
// top-k / streaming-aggregate fast paths land with M18; results here are
// identical (they were pure materialization optimizations).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// runSegment runs one segment over its input rows, recording per-operator
// produced-row counts into prof when profiling (nil = off).
func runSegment(ctx *eval.Ctx, seg *plan.Segment, inputs [][]value.Value, prof *explain.SegProf) [][]value.Value {
	matched := make([][]value.Value, len(inputs))
	for i, inrow := range inputs {
		base := make([]value.Value, seg.RowWidth)
		copy(base, inrow)
		matched[i] = base
	}
	for _, st := range seg.Stages {
		switch s := st.(type) {
		case *plan.MatchStage:
			var opRows []uint64
			if prof != nil {
				opRows = make([]uint64, len(s.Ops)+1)
			}
			matched = runStage(ctx, s, seg.Slots, matched, opRows)
			if prof != nil {
				prof.Stages = append(prof.Stages, explain.StageProf{Match: opRows})
			}
		case *plan.SpStage:
			matched = runSPStage(ctx, s, matched)
			recordSingle(prof, matched)
		case *plan.CallStage:
			matched = runCallStage(ctx, s, matched)
			recordSingle(prof, matched)
		case *plan.UnwindStage:
			matched = runUnwindStage(ctx, s, seg.Slots, matched)
			recordSingle(prof, matched)
		case *plan.CallSubqueryStage:
			matched = runCallSubqueryStage(ctx, s, matched)
			recordSingle(prof, matched)
		}
	}
	var out [][]value.Value
	if seg.Proj.Aggregated {
		out = aggregate(ctx, &seg.Proj, seg.Slots, matched)
	} else {
		out = project(ctx, &seg.Proj, seg.Slots, matched)
	}
	if prof != nil {
		prof.ProjRows = uint64(len(out))
	}
	applyPostWhere(ctx, seg, &out)
	if prof != nil && seg.PostWhere != nil {
		n := uint64(len(out))
		prof.PostWhereRows = &n
	}
	return out
}

// recordSingle records a one-output stage's produced-row count.
func recordSingle(prof *explain.SegProf, matched [][]value.Value) {
	if prof == nil {
		return
	}
	n := uint64(len(matched))
	prof.Stages = append(prof.Stages, explain.StageProf{Single: &n})
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
