// Relationship-type matching and neighbor/rel iteration over a Snapshot:
// the CSR expand ranges, the Neighbors/Rels iter.Seq accessors, and the
// RelMatch pre-resolved filter for hot paths.

package chickpeas

import (
	"iter"
	"slices"

	"github.com/freeeve/gochickpeas/nodeset"
	"github.com/freeeve/gochickpeas/parallel"
)

// RelRef is one relationship incident to a queried node, as yielded by
// Snapshot.Rels. It carries the other endpoint, the type, the direction
// relative to the queried node, and the CSR position for reading the rel's
// properties via Snapshot.RelProp -- valid in BOTH directions (an incoming
// rel's position is pre-mapped to where the property is stored).
type RelRef struct {
	Neighbor  NodeID
	Type      RelType
	Direction Direction
	Pos       uint32
}

// RelMatch is a resolved relationship-type filter. The common single-type
// filter matches with one comparison and no allocation; two or more types
// spill to a slice. Build with Snapshot.Match / MatchType / MatchAll.
type RelMatch struct {
	kind uint8 // 0 = all, 1 = one, 2 = many (possibly empty = match nothing)
	one  RelType
	many []RelType
	// tp is the single type's lazy typed-adjacency holder, populated by
	// Snapshot.Match (not the snapshot-less MatchType): traversals route
	// through the contiguous per-type view when it exists (typedadj.go).
	tp *typedPair
}

// MatchAll matches every relationship type.
func MatchAll() RelMatch {
	return RelMatch{kind: 0}
}

// MatchType matches a single pre-resolved type, allocation-free.
func MatchType(t RelType) RelMatch {
	return RelMatch{kind: 1, one: t}
}

// MatchNone matches no relationship type.
func MatchNone() RelMatch {
	return RelMatch{kind: 2}
}

func (m RelMatch) matches(t RelType) bool {
	switch m.kind {
	case 0:
		return true
	case 1:
		return m.one == t
	}
	return slices.Contains(m.many, t)
}

// Match resolves relationship-type names to a reusable filter: resolve once
// and pass to the *Match traversal methods in a hot loop to skip the
// per-call string lookups. Zero names match every type; unknown names are
// dropped (an unresolvable name has no rels), so all-unknown matches
// nothing. The zero- and one-name paths are allocation-free, so the
// string-typed traversal conveniences stay cheap in hot loops.
func (g *Snapshot) Match(relTypes ...string) RelMatch {
	switch len(relTypes) {
	case 0:
		return MatchAll()
	case 1:
		if t, ok := g.RelType(relTypes[0]); ok {
			return RelMatch{kind: 1, one: t, tp: g.typedPairFor(t)}
		}
		return MatchNone()
	}
	return g.matchMany(relTypes)
}

// matchMany is Match's two-or-more-names path, split out so the common
// zero/one-name paths stay within the inlining budget.
func (g *Snapshot) matchMany(relTypes []string) RelMatch {
	resolved := make([]RelType, 0, len(relTypes))
	for _, name := range relTypes {
		if t, ok := g.RelType(name); ok {
			resolved = append(resolved, t)
		}
	}
	switch len(resolved) {
	case 0:
		return MatchNone()
	case 1:
		return MatchType(resolved[0])
	}
	return RelMatch{kind: 2, many: resolved}
}

// relRange is the CSR rel-index range [start, end) for a node in a
// direction's offset array; empty for ids outside the id space.
func relRange(offsets []uint32, node NodeID) (int, int) {
	i := int(node)
	if i+1 >= len(offsets) {
		return 0, 0
	}
	return int(offsets[i]), int(offsets[i+1])
}

// Degree is the number of relationships incident to node in direction,
// any type -- an O(1) offset difference per side (Both sums the two).
// A runtime fan-out signal for adaptive anchor decisions; for a
// type-restricted count, count Neighbors instead.
func (g *Snapshot) Degree(node NodeID, dir Direction) int {
	n := 0
	if dir == Outgoing || dir == Both {
		lo, hi := relRange(g.outOffsets, node)
		n += hi - lo
	}
	if dir == Incoming || dir == Both {
		lo, hi := relRange(g.inOffsets, node)
		n += hi - lo
	}
	return n
}

// Neighbors iterates the neighbors of node in direction, restricted to the
// given relationship types (zero types match all). Direction Both yields
// matching outgoing neighbors then matching incoming ones; duplicate rels
// yield duplicate neighbors, so counting composes. For per-rel type or
// property access during traversal use Rels.
//
// Both accessors are thin inlinable closure constructors over
// neighborsYield, so a direct `for range` over them is allocation-free
// (the closure and its captures stay on the stack).
func (g *Snapshot) Neighbors(node NodeID, dir Direction, relTypes ...string) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		g.neighborsYield(node, dir, g.Match(relTypes...), yield)
	}
}

// NeighborsMatch is Neighbors over a pre-resolved RelMatch, for hot loops.
func (g *Snapshot) NeighborsMatch(node NodeID, dir Direction, m RelMatch) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		g.neighborsYield(node, dir, m, yield)
	}
}

// AppendNeighborsMatch appends node's matching dir neighbors to dst as a
// deduplicated ASCENDING id set -- the neighbor-ID surface contract
// (parallel same-type rels, or a rel seen from both directions under
// Both, contribute one entry). Per-relationship multiplicity in CSR
// order stays available on AppendNeighborsEach and the Rels iterators.
// Only the appended tail is sorted and compacted; dst's existing prefix
// is untouched.
func (g *Snapshot) AppendNeighborsMatch(dst []NodeID, node NodeID, dir Direction, m RelMatch) []NodeID {
	base := len(dst)
	dst = g.AppendNeighborsEach(dst, node, dir, m)
	tail := dst[base:]
	slices.Sort(tail)
	return append(dst[:base], slices.Compact(tail)...)
}

// AppendNeighborsEach appends one entry per matching dir relationship of
// node, in CSR order -- the traversal primitive behind pattern expansion,
// where relationship multiplicity and first-seen order are semantic. It
// walks the CSR ranges directly rather than through a yield closure:
// neighborsYield is too large to inline, so handing it a closure would
// heap-escape that closure on every call. The walk mirrors
// neighborsYield exactly, minus the early-stop the fill never needs.
func (g *Snapshot) AppendNeighborsEach(dst []NodeID, node NodeID, dir Direction, m RelMatch) []NodeID {
	if dir == Outgoing || dir == Both {
		if tc := m.tp.view(true); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			dst = append(dst, tc.nbrs[lo:hi]...)
		} else {
			lo, hi := relRange(g.outOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(true)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				dst = append(dst, tr.nbrs[rlo:rhi]...)
			} else {
				for k := lo; k < hi; k++ {
					if m.matches(g.outTypes[k]) {
						dst = append(dst, g.outNbrs[k])
					}
				}
			}
		}
	}
	if dir == Incoming || dir == Both {
		if tc := m.tp.view(false); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			dst = append(dst, tc.nbrs[lo:hi]...)
		} else {
			lo, hi := relRange(g.inOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(false)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				dst = append(dst, tr.nbrs[rlo:rhi]...)
			} else {
				for k := lo; k < hi; k++ {
					if m.matches(g.inTypes[k]) {
						dst = append(dst, g.inNbrs[k])
					}
				}
			}
		}
	}
	return dst
}

// neighborsYield walks the CSR ranges, yielding matching neighbors. The
// yield parameter never escapes, keeping range-over-func callers
// allocation-free.
func (g *Snapshot) neighborsYield(node NodeID, dir Direction, m RelMatch, yield func(NodeID) bool) {
	if dir == Outgoing || dir == Both {
		if tc := m.tp.view(true); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			for k := lo; k < hi; k++ {
				if !yield(tc.nbrs[k]) {
					return
				}
			}
		} else {
			lo, hi := relRange(g.outOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(true)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				for k := rlo; k < rhi; k++ {
					if !yield(tr.nbrs[k]) {
						return
					}
				}
			} else {
				for k := lo; k < hi; k++ {
					if m.matches(g.outTypes[k]) && !yield(g.outNbrs[k]) {
						return
					}
				}
			}
		}
	}
	if dir == Incoming || dir == Both {
		if tc := m.tp.view(false); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			for k := lo; k < hi; k++ {
				if !yield(tc.nbrs[k]) {
					return
				}
			}
		} else {
			lo, hi := relRange(g.inOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(false)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				for k := rlo; k < rhi; k++ {
					if !yield(tr.nbrs[k]) {
						return
					}
				}
			} else {
				for k := lo; k < hi; k++ {
					if m.matches(g.inTypes[k]) && !yield(g.inNbrs[k]) {
						return
					}
				}
			}
		}
	}
}

// Rels iterates the relationships incident to node in direction, each
// carrying the CSR position for property reads (see RelRef), outgoing
// matches then incoming ones. Zero types match all. Like Neighbors,
// both accessors are thin inlinable closure constructors, so a direct
// `for range` over them is allocation-free.
func (g *Snapshot) Rels(node NodeID, dir Direction, relTypes ...string) iter.Seq[RelRef] {
	return func(yield func(RelRef) bool) {
		g.relsYield(node, dir, g.Match(relTypes...), yield)
	}
}

// RelsMatch is Rels over a pre-resolved RelMatch, for hot loops.
func (g *Snapshot) RelsMatch(node NodeID, dir Direction, m RelMatch) iter.Seq[RelRef] {
	return func(yield func(RelRef) bool) {
		g.relsYield(node, dir, m, yield)
	}
}

// relsYield walks the CSR ranges, yielding matching RelRefs; yield never
// escapes.
func (g *Snapshot) relsYield(node NodeID, dir Direction, m RelMatch, yield func(RelRef) bool) {
	if dir == Outgoing || dir == Both {
		if tc := m.tp.view(true); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			for k := lo; k < hi; k++ {
				if !yield(RelRef{Neighbor: tc.nbrs[k], Type: m.one, Direction: Outgoing, Pos: tc.poss[k]}) {
					return
				}
			}
		} else {
			lo, hi := relRange(g.outOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(true)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				for k := rlo; k < rhi; k++ {
					if !yield(RelRef{Neighbor: tr.nbrs[k], Type: m.one, Direction: Outgoing, Pos: tr.poss[k]}) {
						return
					}
				}
			} else {
				for k := lo; k < hi; k++ {
					t := g.outTypes[k]
					if !m.matches(t) {
						continue
					}
					if !yield(RelRef{Neighbor: g.outNbrs[k], Type: t, Direction: Outgoing, Pos: uint32(k)}) {
						return
					}
				}
			}
		}
	}
	if dir == Incoming || dir == Both {
		if tc := m.tp.view(false); tc != nil {
			lo, hi := relRange(tc.offsets, node)
			for k := lo; k < hi; k++ {
				if !yield(RelRef{Neighbor: tc.nbrs[k], Type: m.one, Direction: Incoming, Pos: tc.poss[k]}) {
					return
				}
			}
		} else {
			lo, hi := relRange(g.inOffsets, node)
			var tr *typedRuns
			if hi-lo > runScanFloor {
				tr = m.tp.runs(false)
			}
			if tr != nil {
				rlo, rhi := tr.runRange(node)
				for k := rlo; k < rhi; k++ {
					if !yield(RelRef{Neighbor: tr.nbrs[k], Type: m.one, Direction: Incoming, Pos: tr.poss[k]}) {
						return
					}
				}
			} else {
				ito := g.getInToOut() // nil (no rel props) leaves raw positions
				for k := lo; k < hi; k++ {
					t := g.inTypes[k]
					if !m.matches(t) {
						continue
					}
					// Map the incoming position to where the property is stored.
					pos := uint32(k)
					if k < len(ito) {
						pos = ito[k]
					}
					if !yield(RelRef{Neighbor: g.inNbrs[k], Type: t, Direction: Incoming, Pos: pos}) {
						return
					}
				}
			}
		}
	}
}

// FirstNeighbor is the first neighbor of node along the given types in
// direction -- the single-step lookup idiom (a message's creator, a
// person's city). ok is false when there is none.
func (g *Snapshot) FirstNeighbor(node NodeID, dir Direction, relTypes ...string) (NodeID, bool) {
	for n := range g.Neighbors(node, dir, relTypes...) {
		return n, true
	}
	return 0, false
}

// Step is one link of a Follow chain.
type Step struct {
	Dir     Direction
	RelType string
}

// Follow walks a fixed chain of single-rel steps from start, taking the
// first neighbor at each step (e.g. person -> city -> country); ok is false
// as soon as a step has no neighbor.
func (g *Snapshot) Follow(start NodeID, steps ...Step) (NodeID, bool) {
	cur := start
	for _, s := range steps {
		next, ok := g.FirstNeighbor(cur, s.Dir, s.RelType)
		if !ok {
			return 0, false
		}
		cur = next
	}
	return cur, true
}

// HasRel reports whether node has at least one neighbor along the given
// types in direction -- the existence predicate behind "has any X rel".
func (g *Snapshot) HasRel(node NodeID, dir Direction, relTypes ...string) bool {
	_, ok := g.FirstNeighbor(node, dir, relTypes...)
	return ok
}

// NeighborsInSet iterates the typed neighbors of node that are members of
// set -- a label's nodes, a search result, or any precomputed set. Duplicate
// rels are preserved, so it composes with counting.
func (g *Snapshot) NeighborsInSet(node NodeID, dir Direction, set *nodeset.Set, relTypes ...string) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		g.neighborsInSetYield(node, dir, set, g.Match(relTypes...), yield)
	}
}

// neighborsInSetYield is NeighborsInSet's non-inlined walker; yield
// never escapes.
func (g *Snapshot) neighborsInSetYield(node NodeID, dir Direction, set *nodeset.Set, m RelMatch, yield func(NodeID) bool) {
	g.neighborsYield(node, dir, m, func(n NodeID) bool {
		return !set.Contains(n) || yield(n)
	})
}

// HasNeighborWithProperty reports whether any typed neighbor of node in
// direction has property key equal to value (any of string, int, int32,
// int64, float64, bool, or Value). The comparison value resolves to a
// Value once, outside the neighbor scan.
func (g *Snapshot) HasNeighborWithProperty(node NodeID, dir Direction, key string, value any, relTypes ...string) bool {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return false
	}
	want, ok := g.valueFromAny(value)
	if !ok {
		return false
	}
	column, ok := g.columns[keyID]
	if !ok {
		return false
	}
	for n := range g.Neighbors(node, dir, relTypes...) {
		if got, present := column.Get(n); present && got == want {
			return true
		}
	}
	return false
}

// ParNeighborFold folds node's typed neighbors in parallel, merging the
// per-chunk results -- NeighborsMatch composed with a parallel fold. Use
// when each neighbor drives substantial independent work; for light
// per-neighbor work iterate sequentially. The reduce runs in ascending
// chunk order, so the result is deterministic for any associative reduce.
func ParNeighborFold[T any](g *Snapshot, node NodeID, dir Direction, m RelMatch,
	identity func() T, fold func(acc T, neighbor NodeID) T, reduce func(a, b T) T) T {
	var neighbors []NodeID
	for n := range g.NeighborsMatch(node, dir, m) {
		neighbors = append(neighbors, n)
	}
	return parallel.Fold(len(neighbors), identity,
		func(acc T, i int) T { return fold(acc, neighbors[i]) },
		reduce)
}
