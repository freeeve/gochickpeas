// Bound-pair typed-adjacency queries: counting and position-seeking the
// m-matched relationships between two given endpoints. Each side-picks the
// lower-degree endpoint's run (a reverse run lists the same relationships)
// across the typed-view, below-floor run, and sorted edge-key tiers. Split
// from typedadj.go, which holds the view construction.
package chickpeas

import "slices"

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
	// Below the typed-view floor a single type still answers a bound-pair
	// count in O(log E) off its sorted edge-key set: the untyped scan pays
	// the node's FULL degree per probe (every type's relationships), which
	// dominates existence-heavy predicates over small types.
	if m.tp != nil {
		keys := m.tp.edgeKeys()
		a, b := u, v
		if !out {
			a, b = v, u
		}
		return countKeyHits(keys, uint64(a)<<32|uint64(uint32(b)))
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

// edgeKeys lazily builds the type's sorted (src<<32|dst) key multiset from
// one pass over the primary CSR: memory is proportional to the type's
// relationship count (unlike a typed view's id-space offsets), duplicates
// preserve parallel-relationship multiplicity, and one array answers both
// directions (an incoming u->v probe is the key (v, u)).
func (p *typedPair) edgeKeys() []uint64 {
	p.edgeOnce.Do(func() {
		g := p.g
		var count int
		if set, ok := g.typeIndex[p.t]; ok {
			count = set.Len()
		}
		keys := make([]uint64, 0, count)
		for u := 0; u+1 < len(g.outOffsets); u++ {
			for k := g.outOffsets[u]; k < g.outOffsets[u+1]; k++ {
				if g.outTypes[k] == p.t {
					keys = append(keys, uint64(uint32(u))<<32|uint64(uint32(g.outNbrs[k])))
				}
			}
		}
		slices.Sort(keys)
		p.edges = keys
	})
	return p.edges
}

// countKeyHits counts key's occurrences in the sorted key multiset.
func countKeyHits(keys []uint64, key uint64) int {
	lo, _ := slices.BinarySearch(keys, key)
	n := 0
	for i := lo; i < len(keys) && keys[i] == key; i++ {
		n++
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

// AppendRelsBetweenMatch appends the stored CSR position of each m-matched
// dir relationship between u and v to dst, preserving parallel-relationship
// multiplicity -- the bound-both-endpoints position seek behind a named-rel
// rebind expand. Like CountNeighborsMatch it scans whichever endpoint's run
// is shorter (v's reverse run lists the same relationships as u's forward
// run), so the cost is the smaller endpoint's degree rather than always the
// from-side's full degree. Positions come out in the stored (outgoing)
// frame regardless of which side was scanned, so a subsequent property read
// by position is correct. Each direction's appended segment is sorted
// ascending, matching the forward-scan emission order so the seek is
// order-identical to the enumerate-and-filter path.
func (g *Snapshot) AppendRelsBetweenMatch(dst []uint32, u, v NodeID, dir Direction, m RelMatch) []uint32 {
	if dir == Outgoing || dir == Both {
		dst = g.appendDirPosMatch(dst, u, v, true, m)
	}
	if dir == Incoming || dir == Both {
		dst = g.appendDirPosMatch(dst, u, v, false, m)
	}
	return dst
}

// appendDirPosMatch appends one direction's u->v matched relationship
// positions, side-picked by run length. out reports whether u is the source
// (the forward frame); the reverse run of v lists the same relationships.
// The reverse (v-side) scan is taken only when the graph has rel properties
// -- the only state in which a relationship position is ever read (endpoints,
// type, properties all index the outgoing frame), and thus the only state in
// which the incoming->outgoing position map is consulted. With no rel
// properties positions are unread and the forward scan keeps the frame
// identical to the enumerate path.
func (g *Snapshot) appendDirPosMatch(dst []uint32, u, v NodeID, out bool, m RelMatch) []uint32 {
	base := len(dst)
	flipOK := g.hasRelProps
	// Typed-view tier: both endpoints have a contiguous per-type run; scan
	// the shorter one.
	if tcU := m.tp.view(out); tcU != nil {
		loU, hiU := relRange(tcU.offsets, u)
		if tcV := m.tp.view(!out); flipOK && tcV != nil {
			if loV, hiV := relRange(tcV.offsets, v); hiV-loV < hiU-loU {
				dst = appendPosEq(dst, tcV.nbrs, tcV.poss, loV, hiV, u)
				return sortTailU32(dst, base)
			}
		}
		dst = appendPosEq(dst, tcU.nbrs, tcU.poss, loU, hiU, v)
		return sortTailU32(dst, base)
	}
	// Below-floor tier: single type with payload-proportional run views; the
	// run poss carry the stored frame the same way. Side-pick by run length.
	if m.tp != nil {
		if trU := m.tp.runs(out); trU != nil {
			loU, hiU := trU.runRange(u)
			if trV := m.tp.runs(!out); flipOK && trV != nil {
				if loV, hiV := trV.runRange(v); hiV-loV < hiU-loU {
					dst = appendPosEq(dst, trV.nbrs, trV.poss, loV, hiV, u)
					return sortTailU32(dst, base)
				}
			}
			dst = appendPosEq(dst, trU.nbrs, trU.poss, loU, hiU, v)
			return sortTailU32(dst, base)
		}
	}
	// Scan fallback (multi-type / match-all matcher): u's primary run with
	// per-rel type tests, positions mapped to the stored frame for incoming.
	offsets, nbrs, types, posMap := g.outOffsets, g.outNbrs, g.outTypes, []uint32(nil)
	if !out {
		offsets, nbrs, types, posMap = g.inOffsets, g.inNbrs, g.inTypes, g.getInToOut()
	}
	lo, hi := relRange(offsets, u)
	for k := lo; k < hi; k++ {
		if nbrs[k] == v && m.matches(types[k]) {
			pos := uint32(k)
			if k < len(posMap) {
				pos = posMap[k]
			}
			dst = append(dst, pos)
		}
	}
	return dst
}

// appendPosEq appends poss[k] for each k in [lo, hi) whose neighbor equals
// target -- the matching relationships of a bound pair within one scanned
// run.
func appendPosEq(dst []uint32, nbrs []NodeID, poss []uint32, lo, hi int, target NodeID) []uint32 {
	for k := lo; k < hi; k++ {
		if nbrs[k] == target {
			dst = append(dst, poss[k])
		}
	}
	return dst
}

// sortTailU32 sorts dst[base:] ascending in place (a no-op for the common
// single-relationship pair), so a side-picked reverse scan yields the same
// order as the forward scan.
func sortTailU32(dst []uint32, base int) []uint32 {
	if len(dst)-base > 1 {
		slices.Sort(dst[base:])
	}
	return dst
}
