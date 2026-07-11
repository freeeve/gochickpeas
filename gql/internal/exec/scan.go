// Row-independent scan candidate sources: label/property/text-match/
// id-seek/all-nodes (port of the Rust fresh_scan family).
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// scanMatcherRedundant reports whether a fresh scan's per-candidate
// matcher accept is provably redundant: a single-label scan whose op adds
// no extra label or inline property already yields exactly the accepted
// set.
func scanMatcherRedundant(op *plan.BindOp) bool {
	return op.Source.Kind == plan.ScanLabel &&
		len(op.Props) == 0 &&
		len(op.Labels) == 1 && op.Labels[0] == op.Source.Label
}

// freshScan appends a row-independent source's candidates, filtered
// through the op's pre-resolved matcher unless provably redundant.
func freshScan(ctx *eval.Ctx, src *plan.ScanSource, m *graph.NodeMatcher, skipAccept bool, cand *[]graph.NodeID) {
	accept := func(id graph.NodeID) {
		if skipAccept || ctx.G.NodeMatcherAccepts(m, id) {
			*cand = append(*cand, id)
		}
	}
	switch src.Kind {
	case plan.ScanProperty:
		// Resolve first (a param literal reads the context), then serve
		// the anchor from the property index.
		if set := ctx.G.NodesWithProperty(src.Label, src.Key, eval.LitValue(ctx, src.Value)); set != nil {
			for id := range set.Iter() {
				accept(id)
			}
		}
	case plan.ScanLabel:
		if set := ctx.G.NodesWithLabel(src.Label); set != nil {
			for id := range set.Iter() {
				accept(id)
			}
		}
	case plan.ScanNodeID:
		// WHERE id(n) = <int|param>: the kept WHERE conjunct re-verifies,
		// so a non-integer or out-of-id-space value yields no candidate.
		if id, ok := nodeIDSeekValue(ctx, eval.LitValue(ctx, src.Value)); ok {
			accept(id)
		}
	case plan.ScanTextMatch:
		// A substring-index candidate superset when the backend can prune,
		// else the whole label; the kept STARTS WITH/ENDS WITH/CONTAINS
		// conjunct verifies each candidate either way.
		if s, ok := eval.LitValue(ctx, src.Value).AsStr(); ok {
			if set, indexed := ctx.G.SubstringCandidates(src.Label, src.Field, s); indexed {
				if set != nil {
					for id := range set.Iter() {
						accept(id)
					}
				}
				return
			}
		}
		if set := ctx.G.NodesWithLabel(src.Label); set != nil {
			for id := range set.Iter() {
				accept(id)
			}
		}
	case plan.ScanAll:
		for id := graph.NodeID(0); id < ctx.G.IDSpace(); id++ {
			accept(id)
		}
	}
}

// seedFanoutCap bounds one EXISTS seed walk's frontier: past it the walk
// is abandoned and the scan falls back to its base source for the row --
// candidate-superset semantics make the bail free.
const seedFanoutCap = 1 << 17

// existsSeedCandidates enumerates the retained-EXISTS conjuncts'
// candidate superset: each chain walks from its anchor's current row
// value hop by hop over deduplicated ascending neighbor sets,
// label-filtered per level, then the chains union (sort + compact) and
// the op's own matcher applies. The kept WHERE finalizes, so like
// ScanTextMatch this only narrows the scan. Reports false when a walk
// exceeds the fan-out cap (caller falls back to the base source).
func existsSeedCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, seedRel [][]*graph.RelMatcher, seedNode [][]*graph.NodeMatcher, row []value.Value, cand *[]graph.NodeID, scratch *genScratch) bool {
	base := len(*cand)
	for ci := range op.Source.Seeds {
		ch := &op.Source.Seeds[ci]
		a, ok := row[ch.AnchorSlot].AsNode()
		if !ok {
			continue
		}
		frontier := append(scratch.seedFrontier[:0], a)
		next := scratch.seedNext[:0]
		for hi := range ch.Hops {
			next = next[:0]
			for _, u := range frontier {
				next = ctx.G.AppendNeighborsMatched(next, u, ch.Hops[hi].Dir, seedRel[ci][hi])
				if len(next) > seedFanoutCap {
					scratch.seedFrontier, scratch.seedNext = frontier, next
					*cand = (*cand)[:base]
					return false
				}
			}
			slices.Sort(next)
			next = slices.Compact(next)
			kept := next[:0]
			for _, v := range next {
				if ctx.G.NodeMatcherAccepts(seedNode[ci][hi], v) {
					kept = append(kept, v)
				}
			}
			frontier, next = kept, frontier
		}
		*cand = append(*cand, frontier...)
		scratch.seedFrontier, scratch.seedNext = frontier, next
	}
	tail := (*cand)[base:]
	slices.Sort(tail)
	tail = slices.Compact(tail)
	kept := tail[:0]
	for _, id := range tail {
		if ctx.G.NodeMatcherAccepts(m, id) {
			kept = append(kept, id)
		}
	}
	*cand = (*cand)[:base+len(kept)]
	return true
}

// nodeIDSeekValue resolves an id-seek value: an in-id-space non-negative
// integer, comma-ok. The id space (not the node count) bounds it so sparse
// high-id seeds still resolve.
func nodeIDSeekValue(ctx *eval.Ctx, v value.Value) (graph.NodeID, bool) {
	i, ok := v.AsInt()
	if !ok || i < 0 || uint64(i) >= uint64(ctx.G.IDSpace()) {
		return 0, false
	}
	return graph.NodeID(i), true
}
