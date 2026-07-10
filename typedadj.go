// Typed adjacency: a lazily built per-(type, direction) CSR view --
// offsets, neighbors, and original CSR positions restricted to one
// relationship type -- so a single-type traversal reads a contiguous run
// with no per-relationship type test. Snapshot.Match resolves the view
// holder once per compiled stage (one lock there, none per traversal
// call); each direction builds on first traversal via its own Once.
// Per-node relative order matches the primary CSR, so routed results are
// byte-identical to the scan path. Types below the memory floor never
// build (their full offsets array would dwarf their payload) and keep the
// scan path.

package chickpeas

import "sync"

// typedCSR is one direction's single-type view. nil means "not built"
// (below the floor): callers fall back to the type-tested scan.
type typedCSR struct {
	offsets []uint32
	nbrs    []NodeID
	poss    []uint32
}

// typedPair lazily holds both directions' views for one type.
type typedPair struct {
	g       *Snapshot
	t       RelType
	outOnce sync.Once
	inOnce  sync.Once
	out, in *typedCSR
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
			p.in = buildTypedCSR(p.g.inOffsets, p.g.inNbrs, p.g.inTypes, p.g.inToOut, p.t)
		}
	})
	return p.in
}

// typedPairFor returns the shared lazy holder for one type, creating it
// under the snapshot's cache choreography.
func (g *Snapshot) typedPairFor(t RelType) *typedPair {
	g.typedMu.Lock()
	defer g.typedMu.Unlock()
	if p, ok := g.typedAdj[t]; ok {
		return p
	}
	p := &typedPair{g: g, t: t}
	g.typedAdj[t] = p
	return p
}

// CountNeighborsMatch counts the m-matched dir relationships from u to v
// -- the bound-both-endpoints existence/multiplicity probe. Each
// direction scans the lower-degree endpoint's run (v's reverse run lists
// the same relationships), through the typed view when one exists;
// result multiset cardinality is identical to filtering an enumeration.
func (g *Snapshot) CountNeighborsMatch(u, v NodeID, dir Direction, m RelMatch) int {
	n := 0
	if dir == Outgoing || dir == Both {
		n += g.countDirMatch(u, v, true, m)
	}
	if dir == Incoming || dir == Both {
		n += g.countDirMatch(u, v, false, m)
	}
	return n
}

// countDirMatch counts one direction's u->v matches, side-picked by run
// length.
func (g *Snapshot) countDirMatch(u, v NodeID, out bool, m RelMatch) int {
	if tcU := m.tp.view(out); tcU != nil {
		tcV := m.tp.view(!out)
		loU, hiU := relRange(tcU.offsets, u)
		if tcV != nil {
			if loV, hiV := relRange(tcV.offsets, v); hiV-loV < hiU-loU {
				return countHits(tcV.nbrs[loV:hiV], u)
			}
		}
		return countHits(tcU.nbrs[loU:hiU], v)
	}
	// Scan fallback: u's primary run with per-rel type tests.
	offsets, nbrs, types := g.outOffsets, g.outNbrs, g.outTypes
	if !out {
		offsets, nbrs, types = g.inOffsets, g.inNbrs, g.inTypes
	}
	lo, hi := relRange(offsets, u)
	n := 0
	for k := lo; k < hi; k++ {
		if nbrs[k] == v && m.matches(types[k]) {
			n++
		}
	}
	return n
}

// countHits counts occurrences of target in run.
func countHits(run []NodeID, target NodeID) int {
	n := 0
	for _, x := range run {
		if x == target {
			n++
		}
	}
	return n
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
