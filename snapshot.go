// Snapshot: the immutable read-optimized graph -- CSR adjacency (both
// directions), columnar properties, label/type bitmap indexes, and lazily
// built derived indexes. Constructed by the Builder's Finalize or from an
// RCPG file (serialize.go); never mutated afterwards, so reads need no
// locks and the lazy caches synchronize only their builds.

package chickpeas

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/freeeve/gochickpeas/nodeset"
)

// Snapshot is an immutable graph optimized for read-only queries.
//
// The full read surface is deliberately methods on *Snapshot (never free
// functions), so a future cypher package can capture it in a consumer-side
// interface.
type Snapshot struct {
	// nNodes counts distinct nodes -- NOT the CSR id-space size. The offset
	// arrays have one slot per id in 0..=maxNodeID plus a trailing offset,
	// so with sparse ids len(outOffsets)-1 exceeds nNodes; absent ids have
	// empty ranges. Size dense scratch by CSRIDSpace, never NodeCount.
	nNodes uint32
	nRels  uint64

	outOffsets []uint32
	outNbrs    []NodeID
	outTypes   []RelType
	inOffsets  []uint32
	inNbrs     []NodeID
	inTypes    []RelType

	// inToOut maps an incoming CSR position to the outgoing CSR position of
	// the same rel, so rel properties (stored by outgoing position) read
	// correctly for incoming rels. Built lazily by getInToOut on the first
	// rel-POSITION read -- a graph loaded but never queried for rel
	// properties never pays its footprint (~64MB at SF1). hasRelProps is the
	// authority for whether it is ever needed; inToOut stays nil until then.
	inToOut     []uint32
	inToOutOnce sync.Once
	// hasRelProps records whether any relationship column exists -- whether a
	// rel POSITION is ever read. Was proxied by len(inToOut)>0 back when
	// inToOut was materialized eagerly at load.
	hasRelProps bool

	labelIndex map[Label]*nodeset.Set
	typeIndex  map[RelType]*nodeset.Set

	version *string

	columns    map[PropertyKey]Column
	relColumns map[PropertyKey]Column

	atoms *Atoms

	// Lazy caches. propIndex: (label, key) -> value -> node set, built on
	// first equality lookup. colPosIndex/relColPosIndex: position -> slot
	// indexes making sparse-column random reads O(1). All use the same
	// choreography: check under lock, build outside it, re-acquire and keep
	// the first insert (a racing duplicate build is discarded -- both are
	// identical).
	propIndexMu sync.Mutex
	propIndex   map[propIndexKey]map[Value]*nodeset.Set

	colPosMu       sync.Mutex
	colPosIndex    map[PropertyKey]posIndex
	relColPosMu    sync.Mutex
	relColPosIndex map[PropertyKey]posIndex
	rangeMu        sync.Mutex
	rangeIndex     map[PropertyKey]RangeIndex

	// rootsViaIndex caches the forest-root arrays of RootsVia per
	// (direction, rel type); the returned slices index lock-free.
	// functionalVia / terminalOnly cache ChainCollapseVia's structural
	// proofs (resolved per compiled stage, off the per-row path).
	rootsMu       sync.Mutex
	rootsViaIndex map[rootsKey]RootsVia
	functionalVia map[rootsKey]bool
	terminalOnly  map[terminalKey]bool

	// typedAdj caches each rel type's lazy typed-adjacency holder behind
	// an atomic copy-on-write map: Match runs per call on the
	// string-typed traversal conveniences, so the hit path must be
	// lock-free (an earlier mutexed lookup here put a lock acquisition
	// inside kernel neighbor loops -- a 25x native-kernel regression).
	// typedMu serializes only the rare miss's copy-insert (typedadj.go).
	typedMu    sync.Mutex
	typedSlots atomic.Pointer[[typedSlotsLen]atomic.Pointer[typedPair]]
	typedAdj   atomic.Pointer[map[RelType]*typedPair]

	// labelBits caches LabelDense's word bitmaps (resolved per compiled
	// operator, so the mutex is off the per-candidate path).
	labelBitsMu sync.Mutex
	labelBits   map[Label][]uint64

	// fulltextIndex / geoIndex cache the lazily built per-field search
	// indexes; the shared values are cloned out under a brief lock so
	// queries run off the mutex.
	fulltextMu    sync.Mutex
	fulltextIndex map[propIndexKey]*FullTextField
	geoMu         sync.Mutex
	geoIndex      map[geoKey]*GeoIndex

	// relStats builds the per-type count store once on first access.
	relStats func() map[string]RelStats

	// labelDegrees caches per-label conditional degree counts (labelstats.go),
	// built lazily per consulted label.
	labelDegreeMu sync.Mutex
	labelDegrees  map[Label]*labelDegreeEntry
}

// RelStats is the per-type relationship statistics entry: total count and
// the distinct source/target node counts -- the degree facts a cost-based
// planner needs.
type RelStats struct {
	// Count is the total rels of this type.
	Count uint64
	// OutSources is the distinct nodes that are the source of such a rel.
	OutSources uint64
	// InSources is the distinct nodes that are the target of such a rel
	// (the source when traversed incoming).
	InSources uint64
}

// newSnapshot wires the lazy caches; every constructor funnels here.
func newSnapshot() *Snapshot {
	g := &Snapshot{
		outOffsets:     []uint32{0},
		inOffsets:      []uint32{0},
		labelIndex:     map[Label]*nodeset.Set{},
		typeIndex:      map[RelType]*nodeset.Set{},
		columns:        map[PropertyKey]Column{},
		relColumns:     map[PropertyKey]Column{},
		atoms:          NewAtoms([]string{""}),
		propIndex:      map[propIndexKey]map[Value]*nodeset.Set{},
		colPosIndex:    map[PropertyKey]posIndex{},
		relColPosIndex: map[PropertyKey]posIndex{},
		rangeIndex:     map[PropertyKey]RangeIndex{},
		rootsViaIndex:  map[rootsKey]RootsVia{},
		functionalVia:  map[rootsKey]bool{},
		terminalOnly:   map[terminalKey]bool{},
		labelBits:      map[Label][]uint64{},
		fulltextIndex:  map[propIndexKey]*FullTextField{},
		geoIndex:       map[geoKey]*GeoIndex{},
		labelDegrees:   map[Label]*labelDegreeEntry{},
	}
	g.relStats = sync.OnceValue(g.buildRelStats)
	return g
}

// NodeCount is the number of nodes in the graph.
func (g *Snapshot) NodeCount() uint32 {
	return g.nNodes
}

// RelCount is the number of relationships in the graph.
func (g *Snapshot) RelCount() uint64 {
	return g.nRels
}

// getInToOut returns the incoming->outgoing CSR position map, building it once
// on first use (from the retained CSR) so a graph never queried for rel
// properties never materializes it. nil when the graph has no rel columns.
// Synchronized like the typed-view caches: reads after the build see no lock.
func (g *Snapshot) getInToOut() []uint32 {
	if !g.hasRelProps {
		return nil
	}
	g.inToOutOnce.Do(func() {
		if g.inToOut == nil {
			g.inToOut = computeInToOutFromCSR(
				g.outOffsets, g.outNbrs, g.outTypes, g.inOffsets, g.inNbrs, g.inTypes)
		}
	})
	return g.inToOut
}

// CSRIDSpace is the number of id slots in 0..=maxNodeID. Dense scratch
// arrays indexed by raw node id, and loops visiting every source node, must
// size by this -- not NodeCount, which excludes gaps under sparse ids.
func (g *Snapshot) CSRIDSpace() uint32 {
	if len(g.outOffsets) == 0 {
		return 0
	}
	return uint32(len(g.outOffsets) - 1)
}

// buildRelStats scans the outgoing CSR once, counting rels and distinct
// source/target nodes per type.
func (g *Snapshot) buildRelStats() map[string]RelStats {
	type acc struct {
		count    uint64
		src, tgt *nodeset.Set
	}
	byType := map[RelType]*acc{}
	n := int(g.CSRIDSpace())
	for u := 0; u < n; u++ {
		lo, hi := g.outOffsets[u], g.outOffsets[u+1]
		for i := lo; i < hi; i++ {
			t := g.outTypes[i]
			e, ok := byType[t]
			if !ok {
				e = &acc{src: nodeset.New(), tgt: nodeset.New()}
				byType[t] = e
			}
			e.count++
			e.src.Insert(uint32(u))
			e.tgt.Insert(g.outNbrs[i])
		}
	}
	out := make(map[string]RelStats, len(byType))
	for t, e := range byType {
		if name, ok := g.atoms.Resolve(t.ID()); ok {
			out[name] = RelStats{Count: e.count, OutSources: uint64(e.src.Len()), InSources: uint64(e.tgt.Len())}
		}
	}
	return out
}

// RelTypeCount is the total rels of relType (0 if absent). Part of the
// lazily built per-type count store; see AvgDegree.
func (g *Snapshot) RelTypeCount(relType string) uint64 {
	return g.relStats()[relType].Count
}

// RelTypeStats returns the full statistics entry for relType; ok is false
// when the type is absent.
func (g *Snapshot) RelTypeStats(relType string) (RelStats, bool) {
	s, ok := g.relStats()[relType]
	return s, ok
}

// Labels lists the node labels present, sorted by name (schema
// introspection; mirrors db.labels()).
func (g *Snapshot) Labels() []string {
	names := make([]string, 0, len(g.labelIndex))
	for l := range g.labelIndex {
		if name, ok := g.atoms.Resolve(l.ID()); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RelTypes lists the relationship types present, sorted by name (mirrors
// db.relationshipTypes()). For cardinalities use RelTypeCount or
// RelCountByType.
func (g *Snapshot) RelTypes() []string {
	names := make([]string, 0, len(g.typeIndex))
	for t := range g.typeIndex {
		if name, ok := g.atoms.Resolve(t.ID()); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RelTypeCountEntry pairs a relationship type name with its total count.
type RelTypeCountEntry struct {
	Type  string
	Count uint64
}

// RelCountByType lists every type present with its count, sorted by name --
// schema coverage in one pass off the cached count store.
func (g *Snapshot) RelCountByType() []RelTypeCountEntry {
	stats := g.relStats()
	out := make([]RelTypeCountEntry, 0, len(stats))
	for name, s := range stats {
		out = append(out, RelTypeCountEntry{Type: name, Count: s.Count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// AvgDegree is the average fan-out of relType traversed in dir: total such
// rels divided by the distinct nodes having one in that direction (the
// degree-by-type-and-direction a cost-based planner needs). 0 for an absent
// type; Both averages over the nodes touching the type on either side.
func (g *Snapshot) AvgDegree(relType string, dir Direction) float64 {
	s, ok := g.relStats()[relType]
	if !ok {
		return 0
	}
	ratio := func(num, den uint64) float64 {
		if den == 0 {
			return 0
		}
		return float64(num) / float64(den)
	}
	switch dir {
	case Outgoing:
		return ratio(s.Count, s.OutSources)
	case Incoming:
		return ratio(s.Count, s.InSources)
	}
	return ratio(s.Count*2, s.OutSources+s.InSources)
}

// NodesWithLabel is the set of nodes carrying label; ok is false for an
// unknown label. The returned set is shared -- callers must not mutate it
// (Clone first).
func (g *Snapshot) NodesWithLabel(label string) (*nodeset.Set, bool) {
	l, ok := g.Label(label)
	if !ok {
		return nil, false
	}
	set, ok := g.labelIndex[l]
	return set, ok
}

// LabelDense returns a plain word-bitmap over the label's members when
// one is already cached or the label covers at least an eighth of the id
// space, else nil. A dense label's membership probe is then one load and
// mask (words[id>>6]>>(id&63)&1) instead of a compressed-set container
// search -- the per-candidate label test in pattern matching. Built
// lazily once per label; the returned slice is shared and must not be
// mutated. The density floor gates only the BUILD: a bitmap forced by a
// probe-heavy caller (LabelDenseForced) serves every later compile.
func (g *Snapshot) LabelDense(label string) []uint64 {
	l, ok := g.Label(label)
	if !ok {
		return nil
	}
	g.labelBitsMu.Lock()
	if w, ok := g.labelBits[l]; ok {
		g.labelBitsMu.Unlock()
		return w
	}
	g.labelBitsMu.Unlock()
	set, ok := g.labelIndex[l]
	if !ok || set.Len()*8 < int(g.CSRIDSpace()) {
		return nil
	}
	return g.labelDenseBuild(l)
}

// LabelDenseForced builds (or returns) the label's word bitmap ignoring
// the density floor -- for callers that have measured probe volume high
// enough to amortize the id-space-proportional bitmap (an adaptive
// representation choice on observed access volume, never on query
// identity). nil only for an unknown label.
func (g *Snapshot) LabelDenseForced(label string) []uint64 {
	l, ok := g.Label(label)
	if !ok {
		return nil
	}
	if _, ok := g.labelIndex[l]; !ok {
		return nil
	}
	return g.labelDenseBuild(l)
}

// labelDenseBuild builds and caches the label's word bitmap.
func (g *Snapshot) labelDenseBuild(l Label) []uint64 {
	g.labelBitsMu.Lock()
	defer g.labelBitsMu.Unlock()
	if w, ok := g.labelBits[l]; ok {
		return w
	}
	words := make([]uint64, (int(g.CSRIDSpace())+63)/64)
	for id := range g.labelIndex[l].Iter() {
		words[id>>6] |= 1 << (id & 63)
	}
	g.labelBits[l] = words
	return words
}

// RelsWithType is the set of outgoing-CSR positions of rels with relType;
// ok is false for an unknown type. Shared -- callers must not mutate it.
func (g *Snapshot) RelsWithType(relType string) (*nodeset.Set, bool) {
	t, ok := g.RelType(relType)
	if !ok {
		return nil, false
	}
	set, ok := g.typeIndex[t]
	return set, ok
}

// HasLabel reports whether node carries label -- the label-membership test.
func (g *Snapshot) HasLabel(node NodeID, label string) bool {
	set, ok := g.NodesWithLabel(label)
	return ok && set.Contains(node)
}
