// Fixed-hop expansion: one relationship step from a bound node, plus the
// bound-target rebind semijoin (probe a memoized reverse-neighbor set
// instead of re-expanding per row).
package exec

import (
	"slices"

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
	// A native graph filters the appended tail through one batch call (the
	// hot single-dense-label matcher runs as a hoisted bitmap loop with no
	// per-candidate dispatch); the bound-target compaction stays ahead of
	// it, and non-native graphs keep the per-candidate closure.
	sg, native := ctx.G.(*graph.SnapshotGraph)
	// Batch-append the hop into the pooled buffers, then filter the
	// appended tail in place (the iter.Seq forms would heap-allocate their
	// closures on every row -- see Graph.AppendNeighborsMatched). A named
	// relationship variable captures each rel's position alongside its
	// neighbor.
	start := len(*nodes)
	if op.RelSlot != plan.NoSlot {
		// Bound-both-endpoints named expand: the target is fixed, so match it
		// once and seek the (from, bound) relationship positions directly --
		// scanning the lower-degree endpoint -- instead of enumerating from's
		// whole degree and filtering to bound. Each parallel relationship is
		// one appended position (the named side of the enumeration/existence
		// boundary keeps per-relationship bindings observable).
		if native && haveBound {
			if !ctx.G.NodeMatcherAccepts(m, bound) {
				return
			}
			boundPairSeeks++
			relStart := len(*rels)
			*rels = sg.AppendRelsBetween(*rels, fromID, bound, op.Dir, rm)
			for range (*rels)[relStart:] {
				*nodes = append(*nodes, bound)
			}
			return
		}
		relStart := len(*rels)
		*nodes, *rels = ctx.G.AppendRelationshipsMatched(*nodes, *rels, fromID, op.Dir, rm)
		if native {
			*nodes, *rels = sg.FilterMatchedTail(m, *nodes, start, *rels, relStart)
			return
		}
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
	if native {
		if haveBound {
			w := start
			for _, nid := range (*nodes)[start:] {
				if nid == bound {
					(*nodes)[w] = nid
					w++
				}
			}
			*nodes = (*nodes)[:w]
		}
		*nodes, _ = sg.FilterMatchedTail(m, *nodes, start, nil, 0)
		return
	}
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
// so a constant target builds its set once for the whole stage. Each set is
// a sorted id slice probed by binary search -- one allocation per target,
// versus a roaring bitmap's per-range containers for the same membership.
type semiCache map[graph.NodeID][]uint32

// semijoinSetBuilds counts reverse-neighbor-set materializations -- the
// build-once oracle for the semijoin memo: a stage of N rows probing one
// constant target must build exactly ONE set (the invariant neighborhood
// materialized once, membership per row); N builds means the memo is dead.
var semijoinSetBuilds int

// boundPairSeeks counts bound-both-endpoints named-expand position seeks --
// the dispatch oracle for the seek path: a named rebind expand over a bound
// target must reach the seek (this counter climbs) rather than fall back to
// enumerating the from-node's whole degree. A stage of N such rows registers
// N seeks; 0 means the seek is not firing.
var boundPairSeeks int

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

// semijoinCandidates yields the already-bound target once PER qualifying
// relationship from the row's from-node (parallel rels are adjacent
// duplicates in the sorted set, so multiplicity costs one forward walk on
// a hit and nothing on simple graphs), else nothing. Collapsing parallels
// here while the enumerated rebind expand multiplies them made the
// semijoin rewrite result-visible on multigraphs. An empty set
// also stands in for a target failing its own constraints.
func semijoinCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, cache semiCache, row []value.Value, out *[]graph.NodeID, buf *[]graph.NodeID) {
	fromID, ok1 := row[op.From].AsNode()
	target, ok2 := row[op.To].AsNode()
	if !ok1 || !ok2 {
		return
	}
	set, ok := cache[target]
	if !ok {
		if ctx.G.NodeMatcherAccepts(m, target) {
			semijoinSetBuilds++
			set = reverseNeighborSet(ctx, target, op.Dir, rm, buf)
		}
		cache[target] = set
	}
	if i, found := slices.BinarySearch(set, uint32(fromID)); found {
		for j := i; j < len(set) && set[j] == uint32(fromID); j++ {
			*out = append(*out, target)
		}
	}
}

// reverseNeighborSet is the set of nodes with a matching relationship to c
// over dir -- c's neighbors looking back along the flipped direction
// (matchers are direction-independent), collected into a reused buffer and
// returned as a sorted id slice for O(log n) membership probes.
func reverseNeighborSet(ctx *eval.Ctx, c graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, buf *[]graph.NodeID) []uint32 {
	*buf = ctx.G.AppendNeighborsMatched((*buf)[:0], c, flipDir(dir), rm)
	set := make([]uint32, len(*buf))
	copy(set, *buf)
	slices.Sort(set)
	return set
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
