// The chunked final-level seam on the aggregated terminal: the
// engagement counter and floor, the candidate-batch entry with its
// constant-key bulk-count fast path, and the resolved-group fold. Split
// from aggregate_exec.go, which holds per-row update and finalization.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// chunkedFinalPushes / disableChunkedFinal are the chunked final-level
// path's engagement counter and differential pin: the counter proves a
// test exercised the batch path, the toggle pins comparisons to the
// per-row path.
var (
	chunkedFinalPushes  int
	disableChunkedFinal bool
)

// ChunkedFinalPushes exposes the engagement counter to the differential
// harness (a silent decline must fail the test, not pass it vacuously).
func ChunkedFinalPushes() int { return chunkedFinalPushes }

// SetDisableChunkedFinal pins comparisons to the per-row path.
func SetDisableChunkedFinal(v bool) { disableChunkedFinal = v }

// TypedSinkRejects exposes the top-k typed-reject counter: read TOGETHER
// with the engagement counter, a zero-drop run is distinguishable from a
// deleted mechanism (the sibling engine's DESC-case lesson, their 381).
func TypedSinkRejects() int { return typedSinkRejects }

// chunkFloor gates the chunked final level per level-instance: below it
// the per-candidate loop's fixed costs beat the batch setup (the
// round-20 batch-sweep lane regressed IC9 30% on ~39-candidate fills;
// the floor keeps small fills on the per-row path).
const chunkFloor = 64

// pushCandidates consumes a final match level's remaining candidates in
// one call: every candidate is already filtered (the caller guarantees
// swept-or-empty predicates and no compiled filters), so the per-row
// work left is binding and aggregation. When no group key reads the
// bound slot, the whole chunk lands in ONE group: the key packs and
// probes once, and a plain count over the bound variable (or *)
// accumulates len(cands) at once; other aggregate kinds fall back to a
// per-candidate update loop that still skips the per-row sink dispatch.
func (a *aggSink) pushCandidates(row []value.Value, slot int, cands []graph.NodeID) bool {
	agg := a.agg
	if agg.keySlots != nil && !slotsContain(agg.keySlots, slot) {
		var gk64 uint64
		var packed bool
		if len(agg.keySlots) == 1 {
			gk64, packed = packGroupKey1(row[agg.keySlots[0]])
		} else {
			gk64, packed = packGroupKey2(row[agg.keySlots[0]], row[agg.keySlots[1]])
		}
		if packed {
			idx := agg.indexI.GetOrCreate(gk64, func() int {
				agg.keyScratch = agg.keyScratch[:0]
				for _, s := range agg.keySlots {
					agg.keyScratch = append(agg.keyScratch, row[s])
				}
				return agg.appendGroup(agg.keyScratch)
			})
			if a.bulkUpdate(idx, row, slot, cands) {
				return true
			}
			// Mixed aggregate kinds: bind per candidate, update into the
			// resolved group without re-probing.
			states := agg.statesOf(idx)
			var seen []distinctSet
			if agg.hasDistinct {
				seen = agg.seenOf(idx)
			}
			for _, c := range cands {
				row[slot] = value.Node(c)
				a.updateInto(idx, states, seen, row)
			}
			return true
		}
	}
	for _, c := range cands {
		row[slot] = value.Node(c)
		agg.update(a.ctx, row, a.proj, a.slots)
	}
	return true
}

// bulkUpdate applies the whole chunk to group idx when EVERY aggregate
// is a plain count of the bound variable or of *: count += len(cands)
// in one step. Any other kind reports false for the per-candidate loop.
func (a *aggSink) bulkUpdate(idx int, row []value.Value, slot int, cands []graph.NodeID) bool {
	agg := a.agg
	for j := range a.proj.Aggs {
		if agg.kinds[j] != plan.AggCount || a.proj.Aggs[j].Distinct {
			return false
		}
		if agg.aggC[j] != nil && agg.argSlots[j] != slot {
			return false
		}
	}
	states := agg.statesOf(idx)
	for j := range a.proj.Aggs {
		states[j].count += int64(len(cands))
	}
	return true
}

// updateInto is aggregator.update's inner fold with the group already
// resolved -- the chunked path probes once and reuses idx.
func (a *aggSink) updateInto(idx int, states []aggState, seen []distinctSet, row []value.Value) {
	agg := a.agg
	var mm []value.Value
	if agg.hasMinMax {
		mm = agg.mmOf(idx)
	}
	var items [][]value.Value
	if agg.hasCollect {
		items = agg.itemsOf(idx)
	}
	for j := range a.proj.Aggs {
		var arg value.Value
		present := agg.aggC[j] != nil
		if present {
			if s := agg.argSlots[j]; s >= 0 {
				arg = row[s]
			} else {
				arg = agg.aggC[j].Eval(a.ctx, row, a.slots)
			}
		}
		if a.proj.Aggs[j].Distinct && present && !arg.IsNull() {
			if !seen[j].add(arg, &agg.dkScratch) {
				continue
			}
		}
		switch agg.kinds[j] {
		case plan.AggMin, plan.AggMax:
			if arg.IsNull() {
				continue
			}
			if mm[j].IsNull() {
				mm[j] = arg
			} else if c, ok := value.Compare(arg, mm[j]); ok &&
				((agg.kinds[j] == plan.AggMin && c < 0) || (agg.kinds[j] == plan.AggMax && c > 0)) {
				mm[j] = arg
			}
		case plan.AggCollect:
			if !arg.IsNull() {
				items[j] = append(items[j], arg)
			}
		case plan.AggPercentileCont, plan.AggPercentileDisc:
			if _, ok := arg.AsFloat(); ok {
				items[j] = append(items[j], arg)
			}
		default:
			states[j].update(arg, present)
		}
	}
}

// slotsContain reports whether s appears in slots.
func slotsContain(slots []int, s int) bool {
	for _, x := range slots {
		if x == s {
			return true
		}
	}
	return false
}
