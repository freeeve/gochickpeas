// Aggregation execution: routing each matched row into its group's
// accumulators (update), assembling one output row per group with the
// nested-aggregate wrappers, ordering, and pagination (finalize), the
// aggregated terminal sink, and percentile finalization. Split from
// aggregate.go, which holds the aggregator state and group-key packing.
package exec

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// aggTopkBuilds counts group rows materialized on the aggregated bounded
// ORDER BY + LIMIT path (the streaming-selection analogue of
// topkPayloadBuilds); disableAggTopk pins differential tests to the
// materialize-everything-then-sort path.
var (
	aggTopkBuilds  int
	disableAggTopk bool
)

// update routes one matched row into its group. Bare-variable key tuples
// probe the packed index straight off the row slots; a hit skips the key
// evaluation and buffering entirely, while a pack failure or miss takes
// the generic path (whose evaluation yields the identical values).
func (a *aggregator) update(ctx *eval.Ctx, m []value.Value, proj *plan.ProjPlan, slots map[string]int) {
	idx := -1
	if a.keySlots != nil {
		var gk64 uint64
		var packed bool
		if len(a.keySlots) == 1 {
			gk64, packed = packGroupKey1(m[a.keySlots[0]])
		} else {
			gk64, packed = packGroupKey2(m[a.keySlots[0]], m[a.keySlots[1]])
		}
		if packed {
			// One probe covers both outcomes: a miss seeds the new group
			// from the packed key alone (the boxed tuple reconstructs from
			// the pack tags at emission; disablePackedKeys pins the
			// materialized form for differentials).
			idx = a.indexI.GetOrCreate(gk64, func() int {
				if disablePackedKeys {
					a.keyScratch = a.keyScratch[:0]
					for _, s := range a.keySlots {
						a.keyScratch = append(a.keyScratch, m[s])
					}
					return a.appendGroup(a.keyScratch)
				}
				if a.nGroups == 0 {
					a.keysPacked = true
				}
				if a.keysPacked {
					return a.appendGroupPacked(gk64)
				}
				a.keyScratch = a.keyScratch[:0]
				for _, s := range a.keySlots {
					a.keyScratch = append(a.keyScratch, m[s])
				}
				return a.appendGroup(a.keyScratch)
			})
		}
	}
	if idx < 0 {
		a.demoteKeys()
		a.keyScratch = a.keyScratch[:0]
		for _, c := range a.groupC {
			a.keyScratch = append(a.keyScratch, c.Eval(ctx, m, slots))
		}
		idx = a.groupIdx(a.keyScratch)
	}
	states := a.statesOf(idx)
	var seen []distinctSet
	if a.hasDistinct {
		seen = a.seenOf(idx)
	}
	var mm []value.Value
	if a.hasMinMax {
		mm = a.mmOf(idx)
	}
	var num []numAcc
	if a.hasNumAcc {
		num = a.numOf(idx)
	}
	var items [][]value.Value
	if a.hasCollect {
		items = a.itemsOf(idx)
	}
	for j := range proj.Aggs {
		var arg value.Value
		present := a.aggC[j] != nil
		if present {
			if s := a.argSlots[j]; s >= 0 {
				arg = m[s]
			} else {
				arg = a.aggC[j].Eval(ctx, m, slots)
			}
		}
		if proj.Aggs[j].Distinct && present && !arg.IsNull() {
			if !seen[j].add(arg, &a.dkScratch) {
				continue
			}
		}
		switch a.kinds[j] {
		case plan.AggMin, plan.AggMax:
			// The extremum lives on the mm slab; a null slot is the
			// uninitialized sentinel (a min/max arg is never null, so the
			// first non-null arg always seeds it, matching the prior hasMM
			// flag exactly).
			if arg.IsNull() {
				continue
			}
			if mm[j].IsNull() {
				mm[j] = arg
			} else if c, ok := value.Compare(arg, mm[j]); ok &&
				((a.kinds[j] == plan.AggMin && c < 0) || (a.kinds[j] == plan.AggMax && c > 0)) {
				mm[j] = arg
			}
		case plan.AggCollect:
			if !arg.IsNull() {
				items[j] = append(items[j], arg)
			}
		case plan.AggPercentileCont, plan.AggPercentileDisc:
			// Percentiles are over numbers; non-numeric args skip, like avg.
			if _, ok := arg.AsFloat(); ok {
				items[j] = append(items[j], arg)
			}
		default:
			states[j].update(numAt(num, j), arg, present)
		}
	}
}

// numAt returns &num[j] or nil when the slab is absent (count-only).
func numAt(num []numAcc, j int) *numAcc {
	if num == nil {
		return nil
	}
	return &num[j]
}

// postCompile builds the post-aggregation wrapper scope and compiled
// evaluators: wrappers read the hidden accumulator slots as __agg{k} and
// the grouping-key columns by name (the Rust engine's tasks/150); both
// are filled before the wrappers run.
func (a *aggregator) postCompile(ctx *eval.Ctx, proj *plan.ProjPlan) (map[string]int, []RowEval) {
	nCols := len(proj.Returns)
	postSlots := make(map[string]int, proj.NHidden+len(proj.GroupIdx))
	for k := 0; k < proj.NHidden; k++ {
		postSlots[hiddenAggName(k)] = nCols + k
	}
	for _, gi := range proj.GroupIdx {
		if _, ok := postSlots[proj.Returns[gi].Name]; !ok {
			postSlots[proj.Returns[gi].Name] = gi
		}
	}
	postC := make([]RowEval, len(proj.Post))
	for i, p := range proj.Post {
		postC[i] = compileEval(ctx, p.Expr, postSlots)
	}
	return postSlots, postC
}

// forEachGroup finalizes every group into a reused stride-wide scratch
// and hands the visible prefix to fn -- the streaming form of finalize
// for consumers that retain nothing per group (a keyless aggregate over
// no input still emits its zeroed row). The row is only valid for the
// duration of the call.
func (a *aggregator) forEachGroup(ctx *eval.Ctx, proj *plan.ProjPlan, fn func(row []value.Value)) {
	defer a.releasePackedKeys()
	if a.nGroups == 0 && len(proj.GroupIdx) == 0 {
		a.appendGroup(nil)
	}
	nCols := len(proj.Returns)
	postSlots, postC := a.postCompile(ctx, proj)
	stride := nCols + proj.NHidden
	scratch := make([]value.Value, stride)
	a.releaseDistinct()
	for idx := 0; idx < a.nGroups; idx++ {
		clear(scratch)
		a.emitGroup(ctx, proj, idx, scratch, postC, postSlots)
		fn(scratch[:nCols])
	}
}

// finalize emits one row per group (a zeroed row for a keyless aggregate
// over no input), applies the nested-aggregate scalar wrappers over the
// hidden slots, then orders and paginates.
func (a *aggregator) finalize(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) [][]value.Value {
	defer a.releasePackedKeys()
	if a.nGroups == 0 && len(proj.GroupIdx) == 0 {
		a.appendGroup(nil)
	}
	a.releaseDistinct()
	nCols := len(proj.Returns)
	postSlots, postC := a.postCompile(ctx, proj)
	stride := nCols + proj.NHidden
	// The streamed path only pays off when groups can actually be
	// rejected; at nGroups <= bound it would build every row anyway, plus
	// heap bookkeeping the plain sort does not have.
	if bound := orderBound(proj); bound >= 0 && bound < a.nGroups && len(proj.OrderBy) > 0 && !proj.Distinct && !disableAggTopk {
		return a.finalizeTopK(ctx, proj, slots, postC, postSlots, nCols, stride, bound)
	}
	// One arena backs every output row instead of a make per group: a
	// grouping over a million groups then pays one large allocation plus its
	// row-header slice, not a million small ones. Each row is a stride
	// window (nCols visible columns + the hidden accumulator slots the
	// post-wrappers read); only the visible prefix is published.
	arena := make([]value.Value, a.nGroups*stride)
	out := make([][]value.Value, 0, a.nGroups)
	for idx := 0; idx < a.nGroups; idx++ {
		row := arena[idx*stride : idx*stride+stride : idx*stride+stride]
		a.emitGroup(ctx, proj, idx, row, postC, postSlots)
		out = append(out, row[:nCols])
	}
	if len(proj.OrderBy) > 0 {
		out = sortRowsByOrder(ctx, proj, slots, func(int) []value.Value { return nil }, 0, out)
	}
	return paginate(out, proj.Skip, proj.Limit)
}

// emitGroup assembles group idx's output row into row (stride wide: the
// visible columns plus the hidden accumulator slots the post-aggregation
// wrappers read).
func (a *aggregator) emitGroup(ctx *eval.Ctx, proj *plan.ProjPlan, idx int, row []value.Value, postC []RowEval, postSlots map[string]int) {
	if a.keysPacked {
		// Packed regime: reconstruct the tuple straight into the row --
		// value construction is inline, no slab read, no allocation.
		a.keyScratch = appendUnpackedKey(a.keyScratch[:0], a.packedKeys[idx], len(a.groupC))
		for k, gi := range proj.GroupIdx {
			row[gi] = a.keyScratch[k]
		}
	} else {
		keys := a.keysOf(idx)
		for k, gi := range proj.GroupIdx {
			row[gi] = keys[k]
		}
	}
	states := a.statesOf(idx)
	var mm []value.Value
	if a.hasMinMax {
		mm = a.mmOf(idx)
	}
	var num []numAcc
	if a.hasNumAcc {
		num = a.numOf(idx)
	}
	var items [][]value.Value
	if a.hasCollect {
		items = a.itemsOf(idx)
	}
	for j := range proj.Aggs {
		switch a.kinds[j] {
		case plan.AggMin, plan.AggMax:
			// A null slot means no non-null arg was seen -> Null.
			row[proj.Aggs[j].OutIdx] = mm[j]
		case plan.AggCollect:
			row[proj.Aggs[j].OutIdx] = value.List(items[j])
		case plan.AggPercentileCont, plan.AggPercentileDisc:
			row[proj.Aggs[j].OutIdx] = percentileOf(ctx, a.pctC[j], items[j], a.kinds[j] == plan.AggPercentileCont)
		default:
			row[proj.Aggs[j].OutIdx] = states[j].finalize(numAt(num, j))
		}
	}
	for i, p := range proj.Post {
		row[p.Col] = postC[i].Eval(ctx, row, postSlots)
	}
}

// finalizeTopK is finalize's bounded ORDER BY + LIMIT path: each group
// finalizes into a reused stride-wide scratch, its key vector is
// evaluated exactly as sortRowsByOrder would (output column, else the
// key expression over the finalized row under the column scope), and
// only rows the bounded accumulator would admit are copied out -- so at
// most bound group rows materialize instead of all of them, and the
// full-width sort decoration never exists. Selection equals
// sort-then-truncate by topKRows's total order (keys, then group
// arrival, matching the sort's index tiebreak).
func (a *aggregator) finalizeTopK(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, postC []RowEval, postSlots map[string]int, nCols, stride, bound int) [][]value.Value {
	nk := len(proj.OrderBy)
	colIdx := make([]int, nk)
	for k := range proj.OrderBy {
		colIdx[k] = plan.OrderColIndex(proj.OrderBy[k].Expr, proj.Columns, proj.Returns)
	}
	scope := make(map[string]int, len(slots)+len(proj.Columns))
	maps.Copy(scope, slots)
	for i, c := range proj.Columns {
		scope[c] = i
	}
	topk := newTopKRows(bound, nk, proj.OrderBy)
	// Non-column keys compile once and evaluate per group -- the
	// interpreted per-group walk was a measured Q4 cost (two key
	// expressions times every (country, forum) group).
	keyC := make([]RowEval, nk)
	for k := range proj.OrderBy {
		if colIdx[k] < 0 {
			keyC[k] = compileEval(ctx, proj.OrderBy[k].Expr, scope)
		}
	}
	scratch := make([]value.Value, stride)
	kbuf := make([]value.Value, nk)
	// Retention is bounded (plus eviction turnover), so size chunks to the
	// bound instead of paying a full-size chunk for a small LIMIT.
	arena := rowArena{width: nCols, chunkValues: min(arenaChunkValues, max(64, 2*bound)*nCols)}
	for idx := 0; idx < a.nGroups; idx++ {
		clear(scratch)
		a.emitGroup(ctx, proj, idx, scratch, postC, postSlots)
		for k := range proj.OrderBy {
			if ci := colIdx[k]; ci >= 0 {
				kbuf[k] = scratch[ci]
			} else {
				kbuf[k] = keyC[k].Eval(ctx, scratch[:nCols], scope)
			}
		}
		if !topk.wouldAccept(kbuf) {
			topk.seq++ // rejected offers still order future arrivals
			continue
		}
		aggTopkBuilds++
		out := arena.alloc()
		copy(out, scratch[:nCols])
		if !topk.offer(kbuf, out) {
			arena.rollback()
		}
	}
	return paginate(topk.sorted(), proj.Skip, proj.Limit)
}

// hiddenAggName is the rewritten hidden-slot variable a post-aggregation
// wrapper reads (must match the planner's __agg{k} rewrite).
func hiddenAggName(k int) string {
	return "__agg" + strconv.Itoa(k)
}

// aggSink is the aggregated terminal sink: it streams matched rows into
// the group accumulator, so only per-group state is retained.
type aggSink struct {
	ctx   *eval.Ctx
	agg   *aggregator
	proj  *plan.ProjPlan
	slots map[string]int
}

func newAggSink(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) *aggSink {
	return &aggSink{ctx: ctx, agg: newAggregator(ctx, proj, slots), proj: proj, slots: slots}
}

// push always reports true: aggregation must consume every row (the
// necessary asymmetry of the stop protocol).
func (a *aggSink) push(row []value.Value) bool {
	a.agg.update(a.ctx, row, a.proj, a.slots)
	return true
}

func (a *aggSink) close() {}

func (a *aggSink) finalize() [][]value.Value {
	return a.agg.finalize(a.ctx, a.proj, a.slots)
}

// percentileOf finalizes one percentile aggregate: sort the group's
// collected numeric values and pick per Neo4j semantics -- cont
// interpolates linearly between the two straddling values (always Float),
// disc takes the nearest-rank collected value unchanged. An empty group
// or a percentile outside [0,1] is Null.
func percentileOf(ctx *eval.Ctx, pc RowEval, vals []value.Value, cont bool) value.Value {
	if pc == nil || len(vals) == 0 {
		return value.Null()
	}
	p, ok := pc.Eval(ctx, nil, nil).AsFloat()
	if !ok || p < 0 || p > 1 {
		return value.Null()
	}
	slices.SortStableFunc(vals, func(a, b value.Value) int {
		af, _ := a.AsFloat()
		bf, _ := b.AsFloat()
		return cmp.Compare(af, bf)
	})
	n := len(vals)
	if !cont {
		// Nearest rank: ceil(p*n) clamped to [1, n], 1-based.
		idx := int(math.Ceil(p * float64(n)))
		if idx < 1 {
			idx = 1
		}
		return vals[idx-1]
	}
	rank := p * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	lov, _ := vals[lo].AsFloat()
	hiv, _ := vals[hi].AsFloat()
	return value.Float(lov + (hiv-lov)*(rank-float64(lo)))
}
