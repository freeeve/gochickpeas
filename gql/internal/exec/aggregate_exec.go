// Aggregation execution: routing each matched row into its group's
// accumulators (update), assembling one output row per group with the
// nested-aggregate wrappers, ordering, and pagination (finalize), the
// aggregated terminal sink, and percentile finalization. Split from
// aggregate.go, which holds the aggregator state and group-key packing.
package exec

import (
	"cmp"
	"math"
	"slices"
	"strconv"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
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
			// One probe covers both outcomes: a miss materializes the key
			// values (identical to their evaluation -- bare slot reads) only
			// to seed the new group.
			idx = a.indexI.GetOrCreate(gk64, func() int {
				a.keyScratch = a.keyScratch[:0]
				for _, s := range a.keySlots {
					a.keyScratch = append(a.keyScratch, m[s])
				}
				return a.appendGroup(a.keyScratch)
			})
		}
	}
	if idx < 0 {
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
			states[j].update(arg, present)
		}
	}
}

// finalize emits one row per group (a zeroed row for a keyless aggregate
// over no input), applies the nested-aggregate scalar wrappers over the
// hidden slots, then orders and paginates.
func (a *aggregator) finalize(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) [][]value.Value {
	if a.nGroups == 0 && len(proj.GroupIdx) == 0 {
		a.appendGroup(nil)
	}
	nCols := len(proj.Returns)
	// Wrappers read the hidden accumulator slots as __agg{k} and the
	// grouping-key columns by name (the Rust engine's tasks/150); both are
	// filled before the wrappers run.
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
	// One arena backs every output row instead of a make per group: a
	// grouping over a million groups then pays one large allocation plus its
	// row-header slice, not a million small ones. Each row is a stride
	// window (nCols visible columns + the hidden accumulator slots the
	// post-wrappers read); only the visible prefix is published.
	stride := nCols + proj.NHidden
	arena := make([]value.Value, a.nGroups*stride)
	out := make([][]value.Value, 0, a.nGroups)
	for idx := 0; idx < a.nGroups; idx++ {
		row := arena[idx*stride : idx*stride+stride : idx*stride+stride]
		keys := a.keysOf(idx)
		for k, gi := range proj.GroupIdx {
			row[gi] = keys[k]
		}
		states := a.statesOf(idx)
		var mm []value.Value
		if a.hasMinMax {
			mm = a.mmOf(idx)
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
				row[proj.Aggs[j].OutIdx] = states[j].finalize()
			}
		}
		for i, p := range proj.Post {
			row[p.Col] = postC[i].Eval(ctx, row, postSlots)
		}
		out = append(out, row[:nCols])
	}
	if len(proj.OrderBy) > 0 {
		out = sortRowsByOrder(ctx, proj, slots, func(int) []value.Value { return nil }, 0, out)
	}
	return paginate(out, proj.Skip, proj.Limit)
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
