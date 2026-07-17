// Columnar walk-aggregate fusion: a segment whose single MATCH stage is
// a label-scan anchor followed by a pure fresh expand chain, feeding a
// grouped COUNT aggregation over walk-bound entity slots, runs as one
// level-batched column pass -- id columns gathered hop by hop through
// the same AppendNeighborsMatched/FilterMatchedTail/CandidatePred calls
// the row engine makes, with no per-row value boxing, sink dispatch, or
// arena copies. Like the columnar scan-aggregate (colagg.go), the check
// is purely structural -- stage and aggregate shapes -- so any unseen
// query with the same structure fuses identically; anything that
// declines falls back to the general chain, whose results this pass
// reproduces exactly (same candidate calls, same filter buckets, same
// multiset of matched rows).
//
// Level-batched expansion emits groups in level order rather than the
// DFS's encounter order, and the aggregator's group encounter order is
// observable in unordered results -- so eligibility requires the output
// order to be imposed downstream (an ORDER BY on this boundary or on the
// stage-less boundary that consumes it).
package exec

import (
	"os"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
)

// disableColWalk pins differential tests to the general path (settable
// via GQL_DISABLE_COLWALK for A/B measurement); colWalkFired counts
// successful fusions so tests can assert the fused path actually ran.
var (
	disableColWalk = os.Getenv("GQL_DISABLE_COLWALK") != ""
	colWalkFired   int
)

// tryColumnarWalkAgg attempts the fused pass for segments[i]. ok=false
// means the segment (or graph) declined and the general chain runs.
func tryColumnarWalkAgg(ctx *eval.Ctx, segments []*plan.Segment, i int, inputs [][]value.Value) ([][]value.Value, bool) {
	if disableColWalk || len(inputs) != 1 {
		return nil, false
	}
	seg := segments[i]
	proj := &seg.Proj
	if len(seg.Stages) != 1 || !proj.Aggregated || proj.Distinct ||
		len(proj.Post) != 0 || proj.NHidden != 0 {
		return nil, false
	}
	ms, ok := seg.Stages[0].(*plan.MatchStage)
	if !ok || ms.Optional || ms.PathBind != nil || ms.Walk || len(ms.Ops) < 2 {
		return nil, false
	}
	op0 := &ms.Ops[0]
	if op0.Kind != plan.OpScan || op0.Source.Kind != plan.ScanLabel {
		return nil, false
	}
	// The chain: every later op a fresh unnamed untracked expand from an
	// already-bound slot.
	boundAt := map[int]int{op0.Slot: 0}
	for oi := 1; oi < len(ms.Ops); oi++ {
		op := &ms.Ops[oi]
		if op.Kind != plan.OpExpand || op.Rebind || op.RelSlot != plan.NoSlot || op.Uniq != nil {
			return nil, false
		}
		if _, bound := boundAt[op.From]; !bound {
			return nil, false
		}
		if _, dup := boundAt[op.To]; dup {
			return nil, false
		}
		boundAt[op.To] = oi
	}
	// Group keys: bare variables over walk-bound node slots, at most two
	// (the packed accumulation key).
	if len(proj.GroupIdx) > 2 {
		return nil, false
	}
	keyLevels := make([]int, 0, 2)
	for _, gi := range proj.GroupIdx {
		v, ok := proj.Returns[gi].Expr.(*ast.Var)
		if !ok {
			return nil, false
		}
		s, ok := seg.Slots[v.Name]
		if !ok {
			return nil, false
		}
		lv, bound := boundAt[s]
		if !bound {
			return nil, false
		}
		keyLevels = append(keyLevels, lv)
	}
	// Aggregates: plain COUNT only -- count(*) or count of a walk-bound
	// variable, which a matched row always binds non-null, so every
	// aggregate equals the group's row count.
	for _, ac := range proj.Aggs {
		if ac.Kind != plan.AggCount || ac.Distinct {
			return nil, false
		}
		if ac.Arg != nil {
			v, ok := ac.Arg.(*ast.Var)
			if !ok {
				return nil, false
			}
			s, ok := seg.Slots[v.Name]
			if !ok {
				return nil, false
			}
			if _, bound := boundAt[s]; !bound {
				return nil, false
			}
		}
	}
	// Order observability (see package comment).
	ordered := len(proj.OrderBy) > 0
	if !ordered && i+1 < len(segments) {
		next := segments[i+1]
		ordered = len(next.Stages) == 0 && len(next.Proj.OrderBy) > 0
	}
	if !ordered {
		return nil, false
	}
	native, ok := ctx.G.(*graph.SnapshotGraph)
	if !ok {
		return nil, false
	}

	// Compile the stage exactly as the row engine would; decline when any
	// WHERE conjunct needs the general row evaluation, or a semijoin
	// rewrite is in play.
	bound := segmentBoundSlots(seg)
	sample := make([]value.Value, seg.RowWidth)
	copy(sample, inputs[0])
	constIn := func(s int) bool {
		if s < 0 || s >= len(bound) || bound[s] {
			return false
		}
		return slotAgrees(s, inputs, true)
	}
	ctx.MatchEpoch++
	sc := compileStage(ctx, ms, seg.Slots, constIn, sample)
	for oi := range ms.Ops {
		if sc.semijoins[oi] != nil || len(sc.levelFilters[oi]) > 0 {
			return nil, false
		}
	}

	// carryTo[lv] is the last level whose gather still needs level lv's
	// column: as a later hop's source, or as a group key at the end.
	last := len(ms.Ops) - 1
	carryTo := make([]int, len(ms.Ops))
	for oi := 1; oi <= last; oi++ {
		carryTo[boundAt[ms.Ops[oi].From]] = oi - 1
	}
	for _, lv := range keyLevels {
		carryTo[lv] = last
	}

	// Level 0: the anchor scan through the compiled matcher, then its
	// filter bucket.
	var frontier []graph.NodeID
	freshScan(ctx, &op0.Source, sc.matchers[0], scanMatcherRedundant(op0), &frontier)
	frontier = colWalkFilter(ctx, sc, 0, sample, frontier, 0, nil)
	cols := map[int][]graph.NodeID{0: frontier}

	// Hops: gather each level's neighbors per source row, replicating the
	// still-live carried columns alongside.
	var keep []bool
	for oi := 1; oi <= last; oi++ {
		op := &ms.Ops[oi]
		fromLv := boundAt[op.From]
		src := cols[fromLv]
		next := make([]graph.NodeID, 0, len(src)*2)
		carr := map[int][]graph.NodeID{}
		for lv, c := range cols {
			if carryTo[lv] >= oi {
				carr[lv] = make([]graph.NodeID, 0, cap(next))
			}
			_ = c
		}
		for r := range src {
			start := len(next)
			next = ctx.G.AppendNeighborsMatched(next, src[r], op.Dir, sc.relMatchers[oi])
			next, _ = native.FilterMatchedTail(sc.matchers[oi], next, start, nil, 0)
			next = colWalkFilter(ctx, sc, oi, sample, next, start, &keep)
			n := len(next) - start
			if n == 0 {
				continue
			}
			for lv, dst := range carr {
				v := cols[lv][r]
				for k := 0; k < n; k++ {
					dst = append(dst, v)
				}
				carr[lv] = dst
			}
		}
		carr[oi] = next
		cols = carr
	}

	// Accumulate: pack the key ids (a private injective u64 -- two full
	// uint32 ids fit), count rows per group in first-encounter order.
	rows := 0
	for _, c := range cols {
		rows = len(c)
		break
	}
	var idx flatset.U64Map
	var gkA, gkB []graph.NodeID
	var counts []int64
	switch len(keyLevels) {
	case 0:
		counts = append(counts, int64(rows))
	case 1:
		kA := cols[keyLevels[0]]
		for r := 0; r < rows; r++ {
			g := idx.GetOrCreate(uint64(kA[r]), func() int {
				gkA = append(gkA, kA[r])
				counts = append(counts, 0)
				return len(counts) - 1
			})
			counts[g]++
		}
	case 2:
		kA, kB := cols[keyLevels[0]], cols[keyLevels[1]]
		for r := 0; r < rows; r++ {
			g := idx.GetOrCreate(uint64(kA[r])<<32|uint64(kB[r]), func() int {
				gkA = append(gkA, kA[r])
				gkB = append(gkB, kB[r])
				counts = append(counts, 0)
				return len(counts) - 1
			})
			counts[g]++
		}
	}
	// A grouped aggregate over no matches emits no rows; the keyless case
	// above already seeded its zero group.
	nCols := len(proj.Returns)
	out := make([][]value.Value, 0, len(counts))
	cells := make([]value.Value, len(counts)*nCols)
	for g := range counts {
		row := cells[g*nCols : (g+1)*nCols : (g+1)*nCols]
		for k, gi := range proj.GroupIdx {
			if k == 0 {
				row[gi] = value.Node(gkA[g])
			} else {
				row[gi] = value.Node(gkB[g])
			}
		}
		for j := range proj.Aggs {
			row[proj.Aggs[j].OutIdx] = value.Int(counts[g])
		}
		out = append(out, row)
	}
	if len(proj.OrderBy) > 0 {
		out = sortRowsByOrder(ctx, proj, seg.Slots, func(int) []value.Value { return nil }, 0, out)
	}
	out = paginate(out, proj.Skip, proj.Limit)
	if seg.PostWhere != nil {
		applyPostWhere(ctx, seg, &out)
	}
	colWalkFired++
	return out, true
}

// colWalkFilter applies one level's pushed-down filter buckets to the
// candidate tail starting at start, mirroring sweepLevel's batch-then-
// pred order, and returns the compacted buffer.
func colWalkFilter(ctx *eval.Ctx, sc *stageComp, lvl int, row []value.Value, cand []graph.NodeID, start int, keepBuf *[]bool) []graph.NodeID {
	preds := sc.levelPreds[lvl]
	batch := sc.levelBatch[lvl]
	if len(preds) == 0 && len(batch) == 0 {
		return cand
	}
	tail := cand[start:]
	var keep []bool
	if len(batch) > 0 {
		if keepBuf == nil {
			keep = make([]bool, len(tail))
		} else {
			if cap(*keepBuf) < len(tail) {
				*keepBuf = make([]bool, len(tail))
			}
			keep = (*keepBuf)[:len(tail)]
		}
		for i := range keep {
			keep[i] = true
		}
		for _, b := range batch {
			b(ctx, row, tail, keep)
		}
	}
	w := start
	for i, id := range tail {
		if keep != nil && !keep[i] {
			continue
		}
		ok := true
		for _, p := range preds {
			if !p(ctx, row, id) {
				ok = false
				break
			}
		}
		if ok {
			cand[w] = id
			w++
		}
	}
	return cand[:w]
}
