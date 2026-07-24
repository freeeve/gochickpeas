// Delta-aware refinalize: a Builder thawed from a Snapshot tracks which
// components diverge from that source, and Finalize shares -- aliases -- the
// clean ones into the successor instead of rebuilding them. A one-property
// edit no longer costs O(n + m).
//
// The safety argument is the snapshot contract itself: snapshots are strictly
// immutable and the Manager swap is the commit point, so a successor holding
// its source's arrays can never observe a mutation, and Go's GC owns the
// shared backings. Nothing here touches the read path -- traversal still
// scans flat CSR arrays, with no delta overlay or merge-on-read.
//
// Sharing granularity is the component: each CSR direction, each property
// column, each label bitmap, the atom table, and the lazily built indexes
// keyed on them.

package chickpeas

import "maps"

// markNodeColDirty records that key's node column diverges from the source
// snapshot, so Finalize rebuilds it from staged pairs rather than aliasing.
// A no-op on a builder with no source, where nothing can be shared anyway.
func (b *Builder) markNodeColDirty(key PropertyKey) {
	if b.src == nil {
		return
	}
	if b.dirtyNodeCols == nil {
		b.dirtyNodeCols = map[PropertyKey]struct{}{}
	}
	b.dirtyNodeCols[key] = struct{}{}
}

// markRelColDirty records that key's rel column diverges from the source.
func (b *Builder) markRelColDirty(key PropertyKey) {
	if b.src == nil {
		return
	}
	if b.dirtyRelCols == nil {
		b.dirtyRelCols = map[PropertyKey]struct{}{}
	}
	b.dirtyRelCols[key] = struct{}{}
}

// markLabelDirty records that label's node membership diverges from the
// source, so its bitmap rebuilds.
func (b *Builder) markLabelDirty(label Label) {
	if b.src == nil {
		return
	}
	if b.dirtyLabels == nil {
		b.dirtyLabels = map[Label]struct{}{}
	}
	b.dirtyLabels[label] = struct{}{}
}

// markRelsDirty records a rel add or removal. Every rel-derived component --
// both CSR directions, inToOut, the type index, every rel column (which is
// keyed by CSR position), and the forest-root caches -- rebuilds, and the
// source's staging-index -> CSR-position map is dropped with them.
func (b *Builder) markRelsDirty() {
	if b.src == nil {
		return
	}
	b.relsDirty = true
	b.srcRelToOutCSR = nil
}

// aliasPlan is Finalize's one-shot decision of which source components the
// successor may share, plus a record of what actually aliased so the lazy
// caches keyed on those components can carry forward. A builder with no
// source gets a plan that aliases nothing.
type aliasPlan struct {
	b   *Builder
	src *Snapshot

	// csr: both CSR directions, inToOut, and the type index alias.
	csr bool
	// idSpace: the CSR id space is unchanged, so node-column spans -- and
	// with them the dense/rank/sparse layout selection -- match the source.
	idSpace bool
	// labels: no label bitmap diverged, so the whole label index aliases and
	// the O(n) label scan is skipped outright.
	labels bool

	aliasedNodeCols map[PropertyKey]struct{}
	aliasedRelCols  map[PropertyKey]struct{}
	aliasedLabels   map[Label]struct{}
	// allLabels records that finalizeLabels aliased the label index wholesale.
	allLabels bool
}

// newAliasPlan resolves the builder's dirty state against the shape Finalize
// is about to produce: n id slots and m rels.
func (b *Builder) newAliasPlan(n, m int) *aliasPlan {
	p := &aliasPlan{
		b:               b,
		src:             b.src,
		aliasedNodeCols: map[PropertyKey]struct{}{},
		aliasedRelCols:  map[PropertyKey]struct{}{},
		aliasedLabels:   map[Label]struct{}{},
	}
	if b.src == nil {
		return p
	}
	p.idSpace = n == int(b.src.CSRIDSpace())
	// The offset arrays span the id space and the neighbor arrays span the
	// rel count, so a clean rel set is not enough -- a node added past the
	// old maximum (or a retired maximum) rewrites both offset arrays.
	p.csr = !b.relsDirty && p.idSpace && m == len(b.src.outNbrs) && len(b.srcRelToOutCSR) == m
	p.labels = len(b.dirtyLabels) == 0
	return p
}

// nodeColumn returns the source column to share for key, when the column is
// untouched and its span is unchanged. Sharing also carries the column
// bit-identically, which sidesteps the dense-column "never set vs zero"
// conflation that a rebuild through thaw-staged pairs re-applies.
func (p *aliasPlan) nodeColumn(key PropertyKey) (Column, bool) {
	if p.src == nil || !p.idSpace {
		return nil, false
	}
	if _, dirty := p.b.dirtyNodeCols[key]; dirty {
		return nil, false
	}
	col, ok := p.src.columns[key]
	if !ok {
		return nil, false
	}
	p.aliasedNodeCols[key] = struct{}{}
	return col, true
}

// relColumn returns the source rel column to share for key. Rel columns are
// stored by outgoing-CSR position, so they may only alias alongside the CSR.
func (p *aliasPlan) relColumn(key PropertyKey) (Column, bool) {
	if p.src == nil || !p.csr {
		return nil, false
	}
	if _, dirty := p.b.dirtyRelCols[key]; dirty {
		return nil, false
	}
	col, ok := p.src.relColumns[key]
	if !ok {
		return nil, false
	}
	p.aliasedRelCols[key] = struct{}{}
	return col, true
}

// labelClean reports whether label's membership is untouched and the source
// holds a bitmap for it.
func (p *aliasPlan) labelClean(label Label) bool {
	if p.src == nil {
		return false
	}
	if _, dirty := p.b.dirtyLabels[label]; dirty {
		return false
	}
	_, ok := p.src.labelIndex[label]
	return ok
}

// aliasCSR shares both CSR directions and the type index (a pure function of
// the outgoing types) with the source. inToOut follows when the successor has
// rel properties to read through it; when the source had none it was never
// built, and the successor's first rel column pays for it once.
func (p *aliasPlan) aliasCSR(g *Snapshot, hasRelProps bool) {
	src := p.src
	g.outOffsets, g.outNbrs, g.outTypes = src.outOffsets, src.outNbrs, src.outTypes
	g.inOffsets, g.inNbrs, g.inTypes = src.inOffsets, src.inNbrs, src.inTypes
	maps.Copy(g.typeIndex, src.typeIndex)
	g.hasRelProps = hasRelProps
	if !hasRelProps {
		return
	}
	// The successor reads through the SAME CSR positions, so it shares the
	// source's position map rather than deriving an identical copy -- forcing
	// the source's lazy build once and completing g's Once with the shared
	// slice, so getInToOut returns it without re-deriving. (A rebuilt CSR
	// takes a different path and never reaches aliasCSR.)
	shared := src.getInToOut()
	g.inToOutOnce.Do(func() { g.inToOut = shared })
}

// aliasAtoms shares the source atom table when nothing new was interned. The
// interner is seeded from the source and only ever appends, so equal lengths
// mean equal tables -- and skipping the copy matters at LDBC scale, where the
// table holds millions of strings.
func (p *aliasPlan) aliasAtoms(b *Builder) *Atoms {
	if p.src != nil && b.interner.Len() == p.src.atoms.Len() {
		return p.src.atoms
	}
	return b.interner.Atoms()
}

// carryLazyCaches hands the source's built lazy indexes to the successor for
// every entry whose underlying components aliased. propIndex, full-text, geo,
// forest-root, and sparse-column position indexes are expensive builds that
// would otherwise die with the source snapshot at the Manager swap. Entries
// touching a rebuilt component are dropped and rebuild on demand.
//
// The cached values are immutable once built and already shared across
// concurrent queries, so handing out the same pointers is safe; only the
// source's cache maps need their locks while they are read.
func (g *Snapshot) carryLazyCaches(p *aliasPlan) {
	src := p.src
	if src == nil {
		return
	}
	labelAliased := func(l Label) bool {
		if p.allLabels {
			_, ok := src.labelIndex[l]
			return ok
		}
		_, ok := p.aliasedLabels[l]
		return ok
	}
	nodeColAliased := func(k PropertyKey) bool {
		_, ok := p.aliasedNodeCols[k]
		return ok
	}

	src.propIndexMu.Lock()
	for k, index := range src.propIndex {
		// The label-free index (anyLabel) is a pure function of the column.
		if nodeColAliased(k.key) && (k.label == anyLabel || labelAliased(k.label)) {
			g.propIndex[k] = index
		}
	}
	src.propIndexMu.Unlock()

	src.fulltextMu.Lock()
	for k, field := range src.fulltextIndex {
		if nodeColAliased(k.key) && labelAliased(k.label) {
			g.fulltextIndex[k] = field
		}
	}
	src.fulltextMu.Unlock()

	src.geoMu.Lock()
	for k, idx := range src.geoIndex {
		if nodeColAliased(k.latKey) && nodeColAliased(k.lonKey) && labelAliased(k.label) {
			g.geoIndex[k] = idx
		}
	}
	src.geoMu.Unlock()

	if p.csr {
		src.rootsMu.Lock()
		maps.Copy(g.rootsViaIndex, src.rootsViaIndex)
		src.rootsMu.Unlock()
	}

	src.colPosMu.Lock()
	for k, idx := range src.colPosIndex {
		if nodeColAliased(k) {
			g.colPosIndex[k] = idx
		}
	}
	src.colPosMu.Unlock()

	src.relColPosMu.Lock()
	for k, idx := range src.relColPosIndex {
		if _, ok := p.aliasedRelCols[k]; ok {
			g.relColPosIndex[k] = idx
		}
	}
	src.relColPosMu.Unlock()
}
