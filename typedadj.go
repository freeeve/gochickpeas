// Typed adjacency: a lazily built per-(type, direction) CSR view --
// offsets, neighbors, and original CSR positions restricted to one
// relationship type -- so a single-type traversal reads a contiguous run
// with no per-relationship type test. Snapshot.Match resolves the view
// holder once per compiled stage (one lock there, none per traversal
// call); each direction builds on first traversal via its own Once.
// Per-node relative order matches the primary CSR, so routed results are
// byte-identical to the scan path. Types below the memory floor never
// build the id-space offsets array (it would dwarf their payload); they
// route through a payload-proportional run view instead (typedRuns), with
// the type-tested scan remaining the multi-type / MatchAll path.

package chickpeas

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// typedCSR is one direction's single-type view. nil means "not built"
// (below the floor): callers fall back to the type-tested scan.
type typedCSR struct {
	offsets []uint32
	nbrs    []NodeID
	poss    []uint32
}

// typedRuns is one direction's below-floor single-type view: the type's
// relationships filtered from the primary CSR in original order, with the
// owning node per entry instead of id-space offsets -- memory proportional
// to the type's payload. A node's run resolves through the bucket hint: a
// binary search bounded to the node's 64-id bucket, so the probes land in
// the one or two cache lines the bucket's entries span instead of walking
// ~log2(E) cold lines across the whole owner array. Per-node relative
// order matches the primary CSR, so routed results are byte-identical to
// the scan path.
type typedRuns struct {
	nodes []uint32
	nbrs  []NodeID
	poss  []uint32
	// hint[b] is the index in nodes of the first entry whose owner falls in
	// bucket b (owners b<<runHintShift and up); hint[b+1] closes the bucket.
	// One uint32 per 64 id-space slots -- 1/64th of the full offsets array
	// the typed floor exists to avoid -- built read-only with the view, so
	// parallel kernels share it without a single cross-core write.
	hint []uint32
}

// runHintShift buckets the owner array by node id for the run-range
// lookup: 64 ids per bucket keeps the hint at one uint32 per 64 id-space
// slots while bounding each lookup's search to the entries of 64
// consecutive nodes of a below-floor type -- typically a handful, always
// contiguous. The known alternative is a FULL per-id starts array --
// strictly O(1) runRange at ~64x the memory per consulted direction
// (id-space proportional, the very array the typed floor avoids); since
// these views already build lazily per consulted (type, direction), that
// trade is one constant swap away if a CPU profile ever shows the
// bucketed search hot on a hub-heavy workload.
const runHintShift = 6

// runRange is node's [lo, hi) span in the run view: the hint bounds the
// bucket, a binary search inside it finds the run start.
func (r *typedRuns) runRange(node NodeID) (int, int) {
	b := int(node) >> runHintShift
	if b+1 >= len(r.hint) {
		return 0, 0
	}
	lo0, hi0 := int(r.hint[b]), int(r.hint[b+1])
	lo, _ := slices.BinarySearch(r.nodes[lo0:hi0], uint32(node))
	lo += lo0
	hi := lo
	for hi < hi0 && r.nodes[hi] == uint32(node) {
		hi++
	}
	return lo, hi
}

// typedPair lazily holds both directions' views for one type, plus the
// type's sorted edge-key set and below-floor run views -- representations
// sized by the type's payload instead of the id space.
type typedPair struct {
	g               *Snapshot
	t               RelType
	outOnce         sync.Once
	inOnce          sync.Once
	out, in         *typedCSR
	edgeOnce        sync.Once
	edges           []uint64
	outRunsOnce     sync.Once
	inRunsOnce      sync.Once
	outRuns, inRuns *typedRuns
}

// typedFloor: a type builds its view only when its relationship count is
// at least CSRIDSpace/4, keeping the mandatory offsets array (one uint32
// per id-space slot) proportional to the payload it accelerates.
func (g *Snapshot) typedAboveFloor(t RelType) bool {
	set, ok := g.typeIndex[t]
	if !ok {
		return false
	}
	return set.Len()*4 >= int(g.CSRIDSpace())
}

// view returns the direction's typed CSR, building it on first use; nil
// when the type sits below the floor.
func (p *typedPair) view(out bool) *typedCSR {
	if p == nil {
		return nil
	}
	if out {
		p.outOnce.Do(func() {
			if p.g.typedAboveFloor(p.t) {
				p.out = buildTypedCSR(p.g.outOffsets, p.g.outNbrs, p.g.outTypes, nil, p.t)
			}
		})
		return p.out
	}
	p.inOnce.Do(func() {
		if p.g.typedAboveFloor(p.t) {
			// The incoming view bakes the inToOut position mapping in, so
			// property reads index the stored (outgoing) positions directly;
			// an absent mapping keeps raw indexes, mirroring relsYield.
			p.in = buildTypedCSR(p.g.inOffsets, p.g.inNbrs, p.g.inTypes, p.g.getInToOut(), p.t)
		}
	})
	return p.in
}

// runs returns the direction's below-floor run view, building it on first
// use; nil when the type has a full typed CSR instead (at or above the
// floor, where view() serves the traversal). Together the two views cover
// every single-type traversal: contiguous per-type CSR above the floor,
// payload-proportional filtered runs below it.
func (p *typedPair) runs(out bool) *typedRuns {
	if p == nil {
		return nil
	}
	if out {
		p.outRunsOnce.Do(func() {
			if !p.g.typedAboveFloor(p.t) {
				p.outRuns = buildTypedRuns(p.g.outOffsets, p.g.outNbrs, p.g.outTypes, nil, p.t, p.typeCount())
			}
		})
		return p.outRuns
	}
	p.inRunsOnce.Do(func() {
		if !p.g.typedAboveFloor(p.t) {
			p.inRuns = buildTypedRuns(p.g.inOffsets, p.g.inNbrs, p.g.inTypes, p.g.getInToOut(), p.t, p.typeCount())
		}
	})
	return p.inRuns
}

// typeCount is the type's relationship count (0 for an unknown type).
func (p *typedPair) typeCount() int {
	if set, ok := p.g.typeIndex[p.t]; ok {
		return set.Len()
	}
	return 0
}

// buildTypedRuns filters one direction's CSR to a single type in one
// linear pass, keeping the owning node per entry, then indexes the owner
// array with the bucket hint. poss carries each kept relationship's
// property-read position, mapped like buildTypedCSR's.
func buildTypedRuns(offsets []uint32, nbrs []NodeID, types []RelType, posMap []uint32, t RelType, count int) *typedRuns {
	r := &typedRuns{
		nodes: make([]uint32, 0, count),
		nbrs:  make([]NodeID, 0, count),
		poss:  make([]uint32, 0, count),
	}
	n := len(offsets) - 1
	if n < 0 {
		n = 0
	}
	for u := 0; u < n; u++ {
		for k := int(offsets[u]); k < int(offsets[u+1]); k++ {
			if types[k] == t {
				pos := uint32(k)
				if k < len(posMap) {
					pos = posMap[k]
				}
				r.nodes = append(r.nodes, uint32(u))
				r.nbrs = append(r.nbrs, nbrs[k])
				r.poss = append(r.poss, pos)
			}
		}
	}
	r.hint = make([]uint32, (n>>runHintShift)+2)
	bi := 0
	for i, owner := range r.nodes {
		for bi <= int(owner)>>runHintShift {
			r.hint[bi] = uint32(i)
			bi++
		}
	}
	for ; bi < len(r.hint); bi++ {
		r.hint[bi] = uint32(len(r.nodes))
	}
	return r
}

// runScanFloor gates the run view per node: at or below this primary-CSR
// degree the type-tested scan reads the node's whole mixed run in a cache
// line or two, beating the view's lookup; above it the filtered contiguous
// run wins. Callers check the degree before consulting runs().
const runScanFloor = 64

// typedSlotsLen is the dense-id fast path's span: rel-type atoms below it
// resolve through a per-slot atomic slice (two atomic loads, no hashing);
// the rare larger atom falls back to the copy-on-write map. Both paths
// are lock-free on the hit, since the string-typed traversal conveniences
// resolve per call inside kernel loops.
const typedSlotsLen = 4096

// typedPairFor returns the shared lazy holder for one type. Holders are
// created at most once per type (CAS / mutexed copy-insert) and never
// replaced, so a stale read only re-misses.
func (g *Snapshot) typedPairFor(t RelType) *typedPair {
	if t < typedSlotsLen {
		slots := g.typedSlots.Load()
		if slots == nil {
			g.typedMu.Lock()
			if slots = g.typedSlots.Load(); slots == nil {
				slots = &[typedSlotsLen]atomic.Pointer[typedPair]{}
				g.typedSlots.Store(slots)
			}
			g.typedMu.Unlock()
		}
		if p := slots[t].Load(); p != nil {
			return p
		}
		p := &typedPair{g: g, t: t}
		if !slots[t].CompareAndSwap(nil, p) {
			p = slots[t].Load()
		}
		return p
	}
	if m := g.typedAdj.Load(); m != nil {
		if p, ok := (*m)[t]; ok {
			return p
		}
	}
	g.typedMu.Lock()
	defer g.typedMu.Unlock()
	old := g.typedAdj.Load()
	if old != nil {
		if p, ok := (*old)[t]; ok {
			return p
		}
	}
	next := make(map[RelType]*typedPair, 1)
	if old != nil {
		maps.Copy(next, *old)
	}
	p := &typedPair{g: g, t: t}
	next[t] = p
	g.typedAdj.Store(&next)
	return p
}

// buildTypedCSR restricts one direction's CSR to a single type in one
// linear pass, preserving per-node relative order. poss carries each kept
// relationship's property-read position: the raw index, or posMap[index]
// when a mapping is supplied (the incoming direction's inToOut).
func buildTypedCSR(offsets []uint32, nbrs []NodeID, types []RelType, posMap []uint32, t RelType) *typedCSR {
	n := len(offsets) - 1
	if n < 0 {
		return &typedCSR{offsets: []uint32{0}}
	}
	count := 0
	for _, x := range types {
		if x == t {
			count++
		}
	}
	tc := &typedCSR{
		offsets: make([]uint32, n+1),
		nbrs:    make([]NodeID, 0, count),
		poss:    make([]uint32, 0, count),
	}
	for u := 0; u < n; u++ {
		tc.offsets[u] = uint32(len(tc.nbrs))
		for k := int(offsets[u]); k < int(offsets[u+1]); k++ {
			if types[k] == t {
				pos := uint32(k)
				if k < len(posMap) {
					pos = posMap[k]
				}
				tc.nbrs = append(tc.nbrs, nbrs[k])
				tc.poss = append(tc.poss, pos)
			}
		}
	}
	tc.offsets[n] = uint32(len(tc.nbrs))
	return tc
}
