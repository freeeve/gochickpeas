// Builder finalization: the mutable staging area becomes an immutable
// Snapshot -- parallel CSR build (both directions), label/type indexes, and
// per-column dense / rank-select / sparse storage selection. The rank/
// select thresholds and the str dense rule mirror the Rust finalize;
// i64/f64/bool dense selection deliberately diverges to full-coverage-only
// (missingness, tasks/041 -- mirror task filed in the Rust repo), so a
// staged input with partially-filled numeric/bool columns no longer
// finalizes byte-identically with Rust until they converge.

package chickpeas

import (
	"sort"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/internal/bitset"
	"github.com/freeeve/gochickpeas/internal/parallel"
	"github.com/freeeve/gochickpeas/nodeset"
)

// rankSelectMinLen is the minimum column span before a moderately-sparse
// column is stored rank/select instead of binary-searched sparse. Below it
// the sparse array stays cache-friendly and the rank index isn't worth its
// cost.
const rankSelectMinLen = 1_000_000

// rankSelectWorth: the span must be large enough to matter, and the
// rank/select layout (presence bits + packed valueBits-bit values) must not
// use more memory than the sparse pair array (sparsePairBytes per entry,
// including alignment padding).
func rankSelectWorth(span, present, valueBits, sparsePairBytes int) bool {
	if span < rankSelectMinLen {
		return false
	}
	rankBits := span + present*valueBits
	sparseBits := present * sparsePairBytes * 8
	return rankBits <= sparseBits
}

// sortPairsLastWriteWins sorts staged pairs by position keeping the LAST
// staged write per position, so re-set properties resolve deterministically
// to the newest value (the dense fill loop already behaves this way).
func sortPairsLastWriteWins[P any](pairs []P, id func(P) uint32) []P {
	sort.SliceStable(pairs, func(i, j int) bool { return id(pairs[i]) < id(pairs[j]) })
	out := pairs[:0]
	for i, p := range pairs {
		if i+1 < len(pairs) && id(pairs[i+1]) == id(p) {
			continue // a later write for the same position follows
		}
		out = append(out, p)
	}
	return out
}

// denseThreshold (i64/f64/bool): only a FULL column (every position
// staged) stores dense. Those dense layouts have no presence
// representation -- an in-range read always reports present -- so storing
// a partially-filled column dense silently turns "missing" into the zero
// value, and observable property semantics (IS NULL, comparisons,
// aggregates, the monotonic walk) would shift with the storage heuristic.
// A partial column goes to rank-select or sparse, which represent absence
// exactly. This diverges from the Rust finalize's historical >=80% rule
// (tasks/041; a mirror task is filed in the Rust repo). Reading a legacy
// file whose dense column was written at partial fill still reports the
// destroyed positions as present-zero: that information is gone.
//
// pairCount counts staged writes, so duplicate writes to one position can
// reach span without full coverage; the column builders confirm with
// coversAllPositions before committing to dense.
func denseThreshold(pairCount, span int) bool {
	return pairCount >= span
}

// denseStrThreshold: str keeps the historical >=80% rule -- the dense str
// layout encodes missing as atom 0 by design (the read layer folds the
// empty-string check into Prop.Str), so partial fill loses nothing that
// the format doesn't already conflate, and str selection stays
// byte-identical with the Rust finalize.
func denseStrThreshold(pairCount, span int) bool {
	return pairCount >= int(float64(span)*0.8)
}

// coversAllPositions reports whether the pairs touch every position of the
// span (the exactness check behind the dense selection: a write count can
// reach span through duplicates while leaving positions unset).
func coversAllPositions[P interface{ pairID() uint32 }](pairs []P, span int) bool {
	seen := bitset.New(span)
	covered := 0
	for _, p := range pairs {
		if id := int(p.pairID()); !seen.Get(id) {
			seen.Set(id, true)
			covered++
		}
	}
	return covered == span
}

func columnFromPairsI64(pairs []i64Pair, span int) Column {
	if denseThreshold(len(pairs), span) && coversAllPositions(pairs, span) {
		col := make(denseI64Col, span)
		for _, p := range pairs {
			col[p.id] = p.val
		}
		return col
	}
	pairs = sortPairsLastWriteWins(pairs, func(p i64Pair) uint32 { return p.id })
	ids, vals := make([]uint32, len(pairs)), make([]int64, len(pairs))
	for i, p := range pairs {
		ids[i], vals[i] = p.id, p.val
	}
	if rankSelectWorth(span, len(pairs), 64, 16) {
		return rankI64Col{rankIndex: buildRankIndex(ids, span), vals: vals}
	}
	return sparseI64Col{ids: ids, vals: vals}
}

func columnFromPairsF64(pairs []f64Pair, span int) Column {
	if denseThreshold(len(pairs), span) && coversAllPositions(pairs, span) {
		col := make(denseF64Col, span)
		for _, p := range pairs {
			col[p.id] = p.val
		}
		return col
	}
	pairs = sortPairsLastWriteWins(pairs, func(p f64Pair) uint32 { return p.id })
	ids, vals := make([]uint32, len(pairs)), make([]float64, len(pairs))
	for i, p := range pairs {
		ids[i], vals[i] = p.id, p.val
	}
	if rankSelectWorth(span, len(pairs), 64, 16) {
		return rankF64Col{rankIndex: buildRankIndex(ids, span), vals: vals}
	}
	return sparseF64Col{ids: ids, vals: vals}
}

func columnFromPairsBool(pairs []boolPair, span int) Column {
	if denseThreshold(len(pairs), span) && coversAllPositions(pairs, span) {
		col := bitset.New(span)
		for _, p := range pairs {
			col.Set(int(p.id), p.val)
		}
		return denseBoolCol{bits: col}
	}
	pairs = sortPairsLastWriteWins(pairs, func(p boolPair) uint32 { return p.id })
	ids := make([]uint32, len(pairs))
	for i, p := range pairs {
		ids[i] = p.id
	}
	if rankSelectWorth(span, len(pairs), 1, 8) {
		vals := bitset.New(len(pairs))
		for i, p := range pairs {
			vals.Set(i, p.val)
		}
		return rankBoolCol{rankIndex: buildRankIndex(ids, span), vals: vals}
	}
	vals := make([]bool, len(pairs))
	for i, p := range pairs {
		vals[i] = p.val
	}
	return sparseBoolCol{ids: ids, vals: vals}
}

func columnFromPairsStr(pairs []strPair, span int) Column {
	if denseStrThreshold(len(pairs), span) {
		col := make(denseStrCol, span)
		for _, p := range pairs {
			col[p.id] = p.val
		}
		return col
	}
	pairs = sortPairsLastWriteWins(pairs, func(p strPair) uint32 { return p.id })
	ids, vals := make([]uint32, len(pairs)), make([]uint32, len(pairs))
	for i, p := range pairs {
		ids[i], vals[i] = p.id, p.val
	}
	if rankSelectWorth(span, len(pairs), 32, 8) {
		return rankStrCol{rankIndex: buildRankIndex(ids, span), vals: vals}
	}
	return sparseStrCol{ids: ids, vals: vals}
}

// buildCSRDirection fills one CSR direction by counting sort, preserving
// rel insertion order within each node's range, plus the rel-index -> CSR
// position map.
func buildCSRDirection(n int, rels [][2]NodeID, relTypes []RelType, deg []uint32, endpoint int) (
	offsets []uint32, nbrs []NodeID, types []RelType, relToCSR []uint32) {
	m := len(rels)
	offsets = make([]uint32, n+1)
	for i := range n {
		offsets[i+1] = offsets[i] + deg[i]
	}
	nbrs = make([]NodeID, m)
	types = make([]RelType, m)
	relToCSR = make([]uint32, m)
	next := make([]uint32, n)
	copy(next, offsets[:n])
	for idx, r := range rels {
		owner, other := r[endpoint], r[1-endpoint]
		pos := next[owner]
		nbrs[pos] = other
		types[pos] = relTypes[idx]
		relToCSR[idx] = pos
		next[owner]++
	}
	return offsets, nbrs, types, relToCSR
}

// Finalize consumes the builder into an immutable Snapshot; the builder
// must not be used afterwards. indexProperties optionally names property
// keys whose (label, key) equality indexes are built upfront (faster first
// queries, more memory); all others build lazily on first access.
func (b *Builder) Finalize(indexProperties ...string) *Snapshot {
	// Fold pending removals (rel tombstones + detach-delete cascades) into
	// the staging state first; a no-op when nothing was removed.
	b.compactRemovals()
	// The CSR id space covers 0..=maxUsedNodeID (at least one slot).
	n := 1
	if !b.knownNodes.IsEmpty() {
		n = int(b.knownNodes.Maximum()) + 1
	}
	m := len(b.rels)

	g := newSnapshot()
	g.nNodes = uint32(b.knownNodes.GetCardinality())
	g.nRels = uint64(m)
	g.version = b.version

	// The four build phases are independent pure reads of the staging
	// state, so they run as a parallel join; results are deterministic.
	var relToOutCSR, relToInCSR []uint32
	parallel.Join(
		func() {
			g.outOffsets, g.outNbrs, g.outTypes, relToOutCSR =
				buildCSRDirection(n, b.rels, b.relTypes, b.degOut, 0)
		},
		func() {
			g.inOffsets, g.inNbrs, g.inTypes, relToInCSR =
				buildCSRDirection(n, b.rels, b.relTypes, b.degIn, 1)
		},
		func() {
			byLabel := map[Label][]uint32{}
			for id, labels := range b.nodeLabels[:min(n, len(b.nodeLabels))] {
				for _, l := range labels {
					byLabel[l] = append(byLabel[l], uint32(id))
				}
			}
			for l, ids := range byLabel {
				bm := roaring.New()
				bm.AddMany(ids) // ascending and deduped by construction order
				g.labelIndex[l] = nodeset.FromBitmap(bm)
			}
		},
	)
	// Type index by outgoing-CSR position (FORMAT.md section 4), built from
	// the finalized outTypes so it stays correct when rels are staged out of
	// source order -- mirroring the Rust fix (rustychickpeas 96243bb).
	byType := map[RelType][]uint32{}
	for pos, t := range g.outTypes {
		byType[t] = append(byType[t], uint32(pos))
	}
	for t, positions := range byType {
		bm := roaring.New()
		bm.AddMany(positions) // ascending CSR order, duplicate-free
		g.typeIndex[t] = nodeset.FromBitmap(bm)
	}

	for key, pairs := range b.nodeColI64 {
		g.columns[key] = columnFromPairsI64(pairs, n)
	}
	for key, pairs := range b.nodeColF64 {
		g.columns[key] = columnFromPairsF64(pairs, n)
	}
	for key, pairs := range b.nodeColBool {
		g.columns[key] = columnFromPairsBool(pairs, n)
	}
	for key, pairs := range b.nodeColStr {
		g.columns[key] = columnFromPairsStr(pairs, n)
	}

	// Rel properties are stored by outgoing-CSR position: remap the staged
	// rel indexes, and build the incoming -> outgoing position map so
	// incoming traversals read them too (only when rel props exist).
	hasRelProps := len(b.relColI64)+len(b.relColF64)+len(b.relColBool)+len(b.relColStr) > 0
	if hasRelProps {
		g.inToOut = make([]uint32, m)
		for idx := range m {
			g.inToOut[relToInCSR[idx]] = relToOutCSR[idx]
		}
	}
	remap := func(id uint32) uint32 { return relToOutCSR[id] }
	for key, pairs := range b.relColI64 {
		out := make([]i64Pair, len(pairs))
		for i, p := range pairs {
			out[i] = i64Pair{id: remap(p.id), val: p.val}
		}
		g.relColumns[key] = columnFromPairsI64(out, m)
	}
	for key, pairs := range b.relColF64 {
		out := make([]f64Pair, len(pairs))
		for i, p := range pairs {
			out[i] = f64Pair{id: remap(p.id), val: p.val}
		}
		g.relColumns[key] = columnFromPairsF64(out, m)
	}
	for key, pairs := range b.relColBool {
		out := make([]boolPair, len(pairs))
		for i, p := range pairs {
			out[i] = boolPair{id: remap(p.id), val: p.val}
		}
		g.relColumns[key] = columnFromPairsBool(out, m)
	}
	for key, pairs := range b.relColStr {
		out := make([]strPair, len(pairs))
		for i, p := range pairs {
			out[i] = strPair{id: remap(p.id), val: p.val}
		}
		g.relColumns[key] = columnFromPairsStr(out, m)
	}

	g.atoms = b.interner.Atoms()

	// Eagerly build the requested equality indexes from the finished
	// columns -- identical to what the lazy path would build on demand.
	for _, keyName := range indexProperties {
		keyID, ok := g.atoms.ID(keyName)
		if !ok {
			continue
		}
		column, ok := g.columns[keyID]
		if !ok {
			continue
		}
		for l, labelNodes := range g.labelIndex {
			g.propIndex[propIndexKey{label: l, key: keyID}] =
				buildPropValueIndex(column, labelNodes)
		}
	}
	return g
}
