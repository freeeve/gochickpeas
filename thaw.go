// Thaw: reconstruct a mutable Builder from an immutable Snapshot, enabling
// the read-modify-refinalize-swap write loop (thaw an existing snapshot,
// edit it, Finalize a new one, register it in the Manager). Snapshots stay
// immutable throughout; the Manager swap is the commit point.

package chickpeas

import "slices"

// NewBuilderFromSnapshot thaws g back into a Builder whose Finalize
// reproduces g -- a no-edit thaw -> Finalize -> WriteRCPG round trip is
// byte-identical for any snapshot Finalize itself can produce. Atom ids are
// preserved exactly (the interner is seeded from g's atom table), so labels,
// rel types, property keys, and string values stay valid across the thaw.
//
// Two lossy corners are inherited from the snapshot representation itself:
//
//   - Dense columns cannot distinguish "never set" from the zero value, so
//     the thaw stages a pair for every position of a dense column (including
//     atom-0 positions of a dense string column, mirroring the read layer,
//     which reports those as present ""). Refinalize keeps such columns
//     dense unless edits drop them below the 80% threshold, at which point
//     storage selection re-decides.
//   - Ghost nodes -- isolated, unlabeled, propertyless -- leave no
//     identifiable trace. Known nodes rebuild from labels, rel endpoints,
//     and column positions; ghosts outside that union are lost (their count
//     no longer contributes to NodeCount), and dense-column positions inside
//     it may register ids the original builder never saw.
func NewBuilderFromSnapshot(g *Snapshot) *Builder {
	n := int(g.CSRIDSpace())
	m := len(g.outNbrs)
	b := NewBuilder(max(n, 1), max(m, 1))
	b.interner = newInternerFromAtoms(g.atoms)
	if g.version != nil {
		v := *g.version
		b.version = &v
	}

	// Labels: invert the per-label bitmaps into per-node lists, ascending
	// atom order (per-node order was never serialized and Finalize only
	// rebuilds the bitmaps, so atom order is canonical).
	labels := make([]Label, 0, len(g.labelIndex))
	for l := range g.labelIndex {
		labels = append(labels, l)
	}
	slices.Sort(labels)
	for _, l := range labels {
		for id := range g.labelIndex[l].Iter() {
			b.nodeLabels[id] = append(b.nodeLabels[id], l)
			b.knownNodes.Add(id)
		}
	}

	// Rels: restage in a linear extension of BOTH CSR directions' per-node
	// orders, so refinalizing reproduces each CSR byte-identically (naive
	// outgoing order can permute parallel rels within an incoming range).
	outToStaging := thawRels(b, g)

	for key, col := range g.columns {
		thawNodeColumn(b, key, col)
	}
	for key, col := range g.relColumns {
		thawRelColumn(b, key, col, outToStaging)
	}

	if !b.knownNodes.IsEmpty() {
		b.nextNodeID = b.knownNodes.Maximum() + 1
	}
	return b
}

// thawRels stages g's rels in an order consistent with both CSR directions,
// returning the outgoing-CSR position -> staged rel index map (for rel
// column restaging). Each rel goes through the builder's staging core
// (addRelTyped) so degrees, known endpoints, ceilings, and any future
// staging invariant stay owned by one function.
func thawRels(b *Builder, g *Snapshot) []uint32 {
	order := thawRelOrder(g)
	outToStaging := make([]uint32, len(order))
	srcOf := ownersFromOffsets(g.outOffsets)
	for _, outPos := range order {
		u, v := srcOf[outPos], g.outNbrs[outPos]
		idx, err := b.addRelTyped(u, v, g.outTypes[outPos])
		thawMust(err)
		outToStaging[outPos] = uint32(idx)
	}
	return outToStaging
}

// thawMust asserts a staging call that cannot fail during thaw: every id
// and index comes from a consistent snapshot, which already fits the
// builder's ceilings (the builder is pre-sized to the snapshot's id space
// and rel count). A failure here is an engine bug, not caller input.
func thawMust(err error) {
	if err != nil {
		panic("chickpeas: thaw staging invariant violated: " + err.Error())
	}
}

// thawRelOrder computes a staging order (as outgoing-CSR positions) that is
// a linear extension of both CSRs' per-node orders. Each direction imposes a
// chain per node (its CSR range order); the original insertion order is one
// valid extension, so the constraint graph -- at most two predecessors per
// rel -- is acyclic and a Kahn walk recovers an equivalent order. Rels are
// paired across directions by k-th occurrence of (src, dst, type), the same
// pairing computeInToOutFromCSR uses.
func thawRelOrder(g *Snapshot) []uint32 {
	m := len(g.outNbrs)
	if m == 0 {
		return nil
	}
	inToOut := g.inToOut
	if inToOut == nil {
		inToOut = computeInToOutFromCSR(
			g.outOffsets, g.outNbrs, g.outTypes, g.inOffsets, g.inNbrs, g.inTypes)
	}
	outToIn := make([]uint32, m)
	for inPos, outPos := range inToOut {
		outToIn[outPos] = uint32(inPos)
	}
	srcOf := ownersFromOffsets(g.outOffsets)
	dstOf := ownersFromOffsets(g.inOffsets)

	// indeg[p]: one predecessor per chain p is not the head of.
	indeg := make([]uint8, m)
	for p := range m {
		if uint32(p) != g.outOffsets[srcOf[p]] {
			indeg[p]++
		}
		if q := outToIn[p]; q != g.inOffsets[dstOf[q]] {
			indeg[p]++
		}
	}
	queue := make([]uint32, 0, m)
	for p := range m {
		if indeg[p] == 0 {
			queue = append(queue, uint32(p))
		}
	}
	order := make([]uint32, 0, m)
	release := func(p uint32) {
		indeg[p]--
		if indeg[p] == 0 {
			queue = append(queue, p)
		}
	}
	for head := 0; head < len(queue); head++ {
		p := queue[head]
		order = append(order, p)
		if p+1 < g.outOffsets[srcOf[p]+1] {
			release(p + 1)
		}
		if q := outToIn[p]; q+1 < g.inOffsets[dstOf[q]+1] {
			release(inToOut[q+1])
		}
	}
	// A consistent snapshot always yields a complete order; on inconsistent
	// CSRs (a hand-built section whose directions disagree) fall back to
	// outgoing order for the remainder -- the outgoing CSR still round-trips.
	if len(order) < m {
		seen := make([]bool, m)
		for _, p := range order {
			seen[p] = true
		}
		for p := range m {
			if !seen[p] {
				order = append(order, uint32(p))
			}
		}
	}
	return order
}

// ownersFromOffsets expands a CSR offset array into the owning node per
// position.
func ownersFromOffsets(offsets []uint32) []NodeID {
	n := len(offsets) - 1
	if n < 0 {
		return nil
	}
	owners := make([]NodeID, offsets[n])
	for u := range n {
		for p := offsets[u]; p < offsets[u+1]; p++ {
			owners[p] = NodeID(u)
		}
	}
	return owners
}

// thawNodeColumn restages one node column as builder pairs (Entries yields
// every position of a dense column, present positions of sparse and
// rank/select layouts), installed through the builder's bulk staging entry
// so each position registers as a known node under the builder's own
// invariants.
func thawNodeColumn(b *Builder, key PropertyKey, col Column) {
	switch col.Dtype() {
	case DtypeI64:
		pairs := make([]i64Pair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.I64()
			pairs = append(pairs, i64Pair{id: pos, val: x})
		}
		thawMust(setNodeColumnPairs(b, b.nodeColI64, key, pairs))
	case DtypeF64:
		pairs := make([]f64Pair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.F64()
			pairs = append(pairs, f64Pair{id: pos, val: x})
		}
		thawMust(setNodeColumnPairs(b, b.nodeColF64, key, pairs))
	case DtypeBool:
		pairs := make([]boolPair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.Bool()
			pairs = append(pairs, boolPair{id: pos, val: x})
		}
		thawMust(setNodeColumnPairs(b, b.nodeColBool, key, pairs))
	case DtypeStr:
		pairs := make([]strPair, 0, col.Len())
		for pos, v := range col.Entries() {
			atom, _ := v.StrID()
			pairs = append(pairs, strPair{id: pos, val: atom})
		}
		thawMust(setNodeColumnPairs(b, b.nodeColStr, key, pairs))
	}
}

// thawRelColumn restages one rel column, remapping stored outgoing-CSR
// positions to staged rel indexes (Finalize maps them back), installed
// through the builder's bulk staging entry so every id is checked as a
// live staged rel index.
func thawRelColumn(b *Builder, key PropertyKey, col Column, outToStaging []uint32) {
	switch col.Dtype() {
	case DtypeI64:
		pairs := make([]i64Pair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.I64()
			pairs = append(pairs, i64Pair{id: outToStaging[pos], val: x})
		}
		thawMust(setRelColumnPairs(b, b.relColI64, key, pairs))
	case DtypeF64:
		pairs := make([]f64Pair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.F64()
			pairs = append(pairs, f64Pair{id: outToStaging[pos], val: x})
		}
		thawMust(setRelColumnPairs(b, b.relColF64, key, pairs))
	case DtypeBool:
		pairs := make([]boolPair, 0, col.Len())
		for pos, v := range col.Entries() {
			x, _ := v.Bool()
			pairs = append(pairs, boolPair{id: outToStaging[pos], val: x})
		}
		thawMust(setRelColumnPairs(b, b.relColBool, key, pairs))
	case DtypeStr:
		pairs := make([]strPair, 0, col.Len())
		for pos, v := range col.Entries() {
			atom, _ := v.StrID()
			pairs = append(pairs, strPair{id: outToStaging[pos], val: atom})
		}
		thawMust(setRelColumnPairs(b, b.relColStr, key, pairs))
	}
}
