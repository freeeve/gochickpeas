// Fixed-hop expansion: one relationship step from a bound node, plus the
// bound-target rebind semijoin (probe a memoized reverse-neighbor set
// instead of re-expanding per row).
package exec

import (
	"github.com/RoaringBitmap/roaring/v2"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// expandCandidates appends the endpoints of one relationship hop from the
// row's bound from-node, filtered by the op's matcher and (for a rebind)
// the already-bound target; a named relationship also records each rel's
// CSR position parallel to the node.
func expandCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, row []value.Value, nodes *[]graph.NodeID, rels *[]uint32) {
	fromID, ok := row[op.From].AsNode()
	if !ok {
		return
	}
	var bound graph.NodeID
	haveBound := false
	if op.Rebind {
		bound, haveBound = row[op.To].AsNode()
		if !haveBound {
			return
		}
	}
	keep := func(nid graph.NodeID) bool {
		return (!haveBound || bound == nid) && ctx.G.NodeMatcherAccepts(m, nid)
	}
	// Batch-append the hop into the pooled buffers, then filter the
	// appended tail in place (the iter.Seq forms would heap-allocate their
	// closures on every row -- see Graph.AppendNeighborsMatched). A named
	// relationship variable captures each rel's position alongside its
	// neighbor.
	start := len(*nodes)
	if op.RelSlot != plan.NoSlot {
		relStart := len(*rels)
		*nodes, *rels = ctx.G.AppendRelationships(*nodes, *rels, fromID, op.Dir, op.Types)
		w := start
		for i, nb := range (*nodes)[start:] {
			if keep(nb) {
				(*nodes)[w] = nb
				(*rels)[relStart+(w-start)] = (*rels)[relStart+i]
				w++
			}
		}
		*nodes = (*nodes)[:w]
		*rels = (*rels)[:relStart+(w-start)]
		return
	}
	*nodes = ctx.G.AppendNeighborsMatched(*nodes, fromID, op.Dir, rm)
	w := start
	for _, nid := range (*nodes)[start:] {
		if keep(nid) {
			(*nodes)[w] = nid
			w++
		}
	}
	*nodes = (*nodes)[:w]
}

// semiCache memoizes per-target reverse-neighbor sets for one semijoin op,
// so a constant target builds its set once for the whole stage.
type semiCache map[graph.NodeID]*roaring.Bitmap

// buildSemijoins recognizes each bound-target rebind expand with no named
// relationship as an existence semijoin: probe from's membership in
// neighbors(to, flip(dir), types) O(1) per row. Multiplicity-identical to
// the expand on a simple graph; a named relationship keeps the expand so
// per-relationship bindings stay observable.
func buildSemijoins(ops []plan.BindOp) []semiCache {
	out := make([]semiCache, len(ops))
	for i := range ops {
		if ops[i].Kind == plan.OpExpand && ops[i].Rebind && ops[i].RelSlot == plan.NoSlot {
			out[i] = semiCache{}
		}
	}
	return out
}

// semijoinCandidates yields the already-bound target once when the edge
// from the row's from-node exists, else nothing. An empty set also stands
// in for a target failing its own constraints.
func semijoinCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, cache semiCache, row []value.Value, out *[]graph.NodeID) {
	fromID, ok1 := row[op.From].AsNode()
	target, ok2 := row[op.To].AsNode()
	if !ok1 || !ok2 {
		return
	}
	set, ok := cache[target]
	if !ok {
		if ctx.G.NodeMatcherAccepts(m, target) {
			set = reverseNeighborSet(ctx, target, op.Dir, op.Types)
		} else {
			set = roaring.New()
		}
		cache[target] = set
	}
	if set.Contains(uint32(fromID)) {
		*out = append(*out, target)
	}
}

// reverseNeighborSet is the set of nodes with a types relationship to c
// over dir -- c's neighbors looking back along the flipped direction.
func reverseNeighborSet(ctx *eval.Ctx, c graph.NodeID, dir graph.Direction, types []string) *roaring.Bitmap {
	bm := roaring.New()
	for nb := range ctx.G.NeighborsByType(c, flipDir(dir), types) {
		bm.Add(uint32(nb))
	}
	return bm
}

// flipDir is the direction seen when traversing a relationship the other
// way.
func flipDir(dir graph.Direction) graph.Direction {
	switch dir {
	case graph.Outgoing:
		return graph.Incoming
	case graph.Incoming:
		return graph.Outgoing
	}
	return graph.Both
}
