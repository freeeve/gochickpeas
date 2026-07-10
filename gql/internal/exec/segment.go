// Segment runner: build the segment's sink chain (stages in order, then
// the terminal projection/aggregation sink), push each seeded input row
// through it, finalize, and apply the boundary's WHERE. This is the
// streaming form of the Rust M18 fast paths -- results are identical to
// the former stage-by-stage materialization (pure allocation
// optimization).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// terminal is a segment's end sink: it accumulates the projection and
// yields the segment's output rows.
type terminal interface {
	rowSink
	finalize() [][]value.Value
}

// stageProfCell is one stage's PROFILE counters: per-op counts for a MATCH
// stage, a single produced-row count otherwise.
type stageProfCell struct {
	match  []uint64
	single *uint64
}

// runSegment runs one segment over its input rows, recording per-operator
// produced-row counts into prof when profiling (nil = off).
func runSegment(ctx *eval.Ctx, seg *plan.Segment, inputs [][]value.Value, prof *explain.SegProf) [][]value.Value {
	bound := segmentBoundSlots(seg)
	var sample []value.Value
	if len(inputs) > 0 {
		sample = make([]value.Value, seg.RowWidth)
		copy(sample, inputs[0])
	}
	// A slot is batch-constant for hoisting when no stage of the segment
	// binds it and the seeded inputs agree on its value (a slot beyond an
	// input's width seeds as Null).
	constIn := func(s int) bool {
		if s < 0 || s >= len(bound) || bound[s] {
			return false
		}
		return slotAgrees(s, inputs, true)
	}

	var term terminal
	if seg.Proj.Aggregated {
		term = newAggSink(ctx, &seg.Proj, seg.Slots)
	} else {
		term = newProjSink(ctx, &seg.Proj, seg.Slots, seg.RowWidth)
	}
	var sink rowSink = term
	profCells := make([]stageProfCell, len(seg.Stages))
	// One used-relationship env per chain: a MATCH clause's uniqueness
	// scope spans its chained stages (comma patterns, planner splits), so
	// every matchSink of this segment shares the stack; the DFS push/pop
	// discipline empties it between rows.
	uniq := &uniqEnv{}
	for i := len(seg.Stages) - 1; i >= 0; i-- {
		var cell *stageProfCell
		if prof != nil {
			cell = &profCells[i]
		}
		sink = buildStageSink(ctx, seg, seg.Stages[i], sink, constIn, sample, cell, uniq)
	}

	buf := make([]value.Value, seg.RowWidth)
	for _, in := range inputs {
		clear(buf)
		copy(buf, in)
		sink.push(buf)
	}
	sink.close()
	out := term.finalize()

	if prof != nil {
		for _, c := range profCells {
			if c.match != nil {
				prof.Stages = append(prof.Stages, explain.StageProf{Match: c.match})
			} else {
				prof.Stages = append(prof.Stages, explain.StageProf{Single: c.single})
			}
		}
		prof.ProjRows = uint64(len(out))
	}
	applyPostWhere(ctx, seg, &out)
	if prof != nil && seg.PostWhere != nil {
		n := uint64(len(out))
		prof.PostWhereRows = &n
	}
	return out
}

// buildStageSink wires one stage into the chain as a row sink feeding
// next, registering its PROFILE counter when cell is non-nil.
func buildStageSink(ctx *eval.Ctx, seg *plan.Segment, st plan.Stage, next rowSink, constIn func(int) bool, sample []value.Value, cell *stageProfCell, uniq *uniqEnv) rowSink {
	single := func() *uint64 {
		if cell == nil {
			return nil
		}
		cell.single = new(uint64)
		return cell.single
	}
	switch s := st.(type) {
	case *plan.MatchStage:
		ms := &matchSink{
			ctx: ctx, stage: s, comp: compileStage(ctx, s, seg.Slots, constIn, sample),
			slots: seg.Slots, buf: make([]value.Value, seg.RowWidth), next: next, uniq: uniq,
		}
		ms.emitFn = ms.emit
		if s.Optional {
			ms.orig = make([]value.Value, seg.RowWidth)
		}
		if s.PathBind != nil && s.Where != nil {
			var conjs []ast.Expr
			plan.SplitAnd(s.Where, &conjs)
			for _, c := range conjs {
				ms.pathFilters = append(ms.pathFilters, compileEval(ctx, c, seg.Slots))
			}
		}
		if cell != nil {
			ms.opRows = make([]uint64, len(s.Ops)+1)
			cell.match = ms.opRows
		}
		return ms
	case *plan.SpStage:
		return &spSink{ctx: ctx, sp: s, arena: rowArena{width: seg.RowWidth}, next: next, count: single()}
	case *plan.CallStage:
		cs := &callSink{ctx: ctx, cs: s, buf: make([]value.Value, seg.RowWidth), next: next, count: single()}
		if native, ok := ctx.G.(graph.Native); ok {
			cs.native = true
			cs.g = native.Snapshot()
			if len(s.ArgExprs) > 0 {
				cs.argEvals = make([]RowEval, len(s.ArgExprs))
				for i, a := range s.ArgExprs {
					cs.argEvals[i] = compileEval(ctx, a, seg.Slots)
				}
				cs.slots = seg.Slots
			} else {
				cs.values, cs.hits, cs.prop = callResults(&s.Proc, cs.g)
			}
		}
		return cs
	case *plan.UnwindStage:
		return &unwindSink{
			ctx: ctx, list: compileEval(ctx, s.List, seg.Slots), slots: seg.Slots,
			out: s.OutSlot, buf: make([]value.Value, seg.RowWidth), next: next, count: single(),
		}
	default:
		s2 := st.(*plan.CallSubqueryStage)
		return &subquerySink{
			ctx: ctx, cs: s2, buf: make([]value.Value, seg.RowWidth),
			seed: make([]value.Value, len(s2.ImportSlots)), next: next, count: single(),
		}
	}
}

// segmentBoundSlots marks every slot bound by any stage of the segment;
// those can never be hoisting-constant even when the seeded inputs agree.
func segmentBoundSlots(seg *plan.Segment) []bool {
	b := make([]bool, seg.RowWidth)
	set := func(s int) {
		if s >= 0 && s < len(b) {
			b[s] = true
		}
	}
	for _, st := range seg.Stages {
		switch s := st.(type) {
		case *plan.MatchStage:
			for i := range s.Ops {
				op := &s.Ops[i]
				set(slotOf(op))
				if op.Kind == plan.OpExpand || op.Kind == plan.OpVarExpand {
					set(op.RelSlot)
				}
			}
			if s.PathBind != nil {
				set(s.PathBind.PathSlot)
			}
		case *plan.SpStage:
			set(s.PathSlot)
		case *plan.CallStage:
			set(s.NodeSlot)
			set(s.ValueSlot)
		case *plan.UnwindStage:
			set(s.OutSlot)
		case *plan.CallSubqueryStage:
			for _, o := range s.OutSlots {
				set(o)
			}
		}
	}
	return b
}

// streamableBoundary reports whether seg's projection boundary is pure
// per-row passthrough -- no aggregation, DISTINCT, ORDER BY, or
// pagination -- so its rows can seed the next segment as they are
// produced instead of materializing between segments.
func streamableBoundary(seg *plan.Segment) bool {
	p := &seg.Proj
	return !p.Aggregated && !p.Distinct && len(p.OrderBy) == 0 &&
		p.Skip == nil && p.Limit == nil && len(p.Post) == 0 && p.NHidden == 0
}

// passthroughSink is a streamed projection boundary: it projects the
// upstream segment's output columns, applies the boundary WHERE, and
// re-seeds the downstream chain's input buffer per row. Only built for
// streamableBoundary segments, where this is value-identical to
// finalize-then-reseed.
type passthroughSink struct {
	ctx      *eval.Ctx
	returns  []RowEval
	slots    map[string]int
	where    RowEval
	colScope map[string]int
	out      []value.Value
	buf      []value.Value
	next     rowSink
}

func newPassthroughSink(ctx *eval.Ctx, seg *plan.Segment, nextWidth int, next rowSink) *passthroughSink {
	p := &passthroughSink{
		ctx:     ctx,
		returns: make([]RowEval, len(seg.Proj.Returns)),
		slots:   seg.Slots,
		out:     make([]value.Value, len(seg.Proj.Returns)),
		buf:     make([]value.Value, nextWidth),
		next:    next,
	}
	for i, r := range seg.Proj.Returns {
		p.returns[i] = compileEval(ctx, r.Expr, seg.Slots)
	}
	if seg.PostWhere != nil {
		p.colScope = make(map[string]int, len(seg.Proj.Columns))
		for i, c := range seg.Proj.Columns {
			p.colScope[c] = i
		}
		p.where = compileEval(ctx, seg.PostWhere, p.colScope)
	}
	return p
}

func (p *passthroughSink) push(row []value.Value) {
	for i, c := range p.returns {
		p.out[i] = c.Eval(p.ctx, row, p.slots)
	}
	if p.where != nil && !p.where.Eval(p.ctx, p.out, p.colScope).IsTruthy() {
		return
	}
	clear(p.buf)
	copy(p.buf, p.out)
	p.next.push(p.buf)
}

func (p *passthroughSink) close() { p.next.close() }

// buildChain wires a segment's stages onto term, returning the head sink
// (no PROFILE cells: streaming runs are unprofiled by construction).
func buildChain(ctx *eval.Ctx, seg *plan.Segment, term rowSink, constIn func(int) bool, sample []value.Value) rowSink {
	uniq := &uniqEnv{}
	sink := term
	for i := len(seg.Stages) - 1; i >= 0; i-- {
		sink = buildStageSink(ctx, seg, seg.Stages[i], sink, constIn, sample, nil, uniq)
	}
	return sink
}

// runSegmentRun executes a run of segments whose interior boundaries are
// all streamable: every non-final segment's terminal becomes a
// passthrough into the next segment's chain, so rows cross boundaries as
// they are produced and only the final terminal retains state. The first
// segment keeps the materialized-inputs batch-constant analysis; the
// streamed-into segments compile conservatively (nothing batch-constant
// -- a perf-only distinction, cInCarried still applies per epoch).
func runSegmentRun(ctx *eval.Ctx, segs []*plan.Segment, inputs [][]value.Value) [][]value.Value {
	last := segs[len(segs)-1]
	if len(segs) == 1 {
		return runSegment(ctx, last, inputs, nil)
	}
	never := func(int) bool { return false }
	var term terminal
	if last.Proj.Aggregated {
		term = newAggSink(ctx, &last.Proj, last.Slots)
	} else {
		term = newProjSink(ctx, &last.Proj, last.Slots, last.RowWidth)
	}
	head := buildChain(ctx, last, term, never, nil)
	for k := len(segs) - 2; k >= 0; k-- {
		seg := segs[k]
		pt := newPassthroughSink(ctx, seg, segs[k+1].RowWidth, head)
		constIn := never
		var sample []value.Value
		if k == 0 {
			bound := segmentBoundSlots(seg)
			if len(inputs) > 0 {
				sample = make([]value.Value, seg.RowWidth)
				copy(sample, inputs[0])
			}
			constIn = func(s int) bool {
				if s < 0 || s >= len(bound) || bound[s] {
					return false
				}
				return slotAgrees(s, inputs, true)
			}
		}
		head = buildChain(ctx, seg, pt, constIn, sample)
	}
	buf := make([]value.Value, segs[0].RowWidth)
	for _, in := range inputs {
		clear(buf)
		copy(buf, in)
		head.push(buf)
	}
	head.close()
	out := term.finalize()
	applyPostWhere(ctx, last, &out)
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
	isConst := func(s int) bool { return slotAgrees(s, rows, false) }
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
