// SnapshotGraph adapts *chickpeas.Snapshot to the Graph seam, forwarding
// each method to the engine's read surface and resolving stored properties
// to runtime values.
package graph

import (
	"iter"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// SnapshotGraph is the native Graph implementation over an immutable
// engine snapshot.
type SnapshotGraph struct {
	g *chickpeas.Snapshot
}

// New wraps an engine snapshot as a Graph.
func New(g *chickpeas.Snapshot) *SnapshotGraph { return &SnapshotGraph{g: g} }

// Snapshot exposes the underlying engine snapshot (the Native capability
// for kernel offload and the columnar compiled eval path).
func (s *SnapshotGraph) Snapshot() *chickpeas.Snapshot { return s.g }

// NodeCount is the number of distinct node ids.
func (s *SnapshotGraph) NodeCount() uint32 { return s.g.NodeCount() }

// IDSpace is the CSR id-space upper bound (exceeds NodeCount under sparse
// ids).
func (s *SnapshotGraph) IDSpace() uint32 { return s.g.CSRIDSpace() }

// AvgDegree forwards the engine's per-type degree statistic.
func (s *SnapshotGraph) AvgDegree(relType string, dir chickpeas.Direction) float64 {
	return s.g.AvgDegree(relType, dir)
}

// Degree forwards the O(1) untyped incident-relationship count.
func (s *SnapshotGraph) Degree(node chickpeas.NodeID, dir chickpeas.Direction) int {
	return s.g.Degree(node, dir)
}

// HasLabel reports whether node carries label.
func (s *SnapshotGraph) HasLabel(node chickpeas.NodeID, label string) bool {
	return s.g.HasLabel(node, label)
}

// NodesWithLabel is the shared label-index set; nil when the label is
// unknown.
func (s *SnapshotGraph) NodesWithLabel(label string) *nodeset.Set {
	set, ok := s.g.NodesWithLabel(label)
	if !ok {
		return nil
	}
	return set
}

// LabelCardinality answers from the label index without materializing ids.
func (s *SnapshotGraph) LabelCardinality(label string) uint64 {
	set, ok := s.g.NodesWithLabel(label)
	if !ok {
		return 0
	}
	return uint64(set.Len())
}

// NodesWithProperty serves the {key: value} anchor from the engine's lazy
// inverted property index, dispatched on the value's scalar kind. A
// non-scalar value (or a string absent from the interner) matches nothing.
func (s *SnapshotGraph) NodesWithProperty(label, key string, v value.Value) *nodeset.Set {
	ev, ok := engineValue(s.g, v)
	if !ok {
		return nil
	}
	set, ok := s.g.NodesWithValue(label, key, ev)
	if !ok {
		return nil
	}
	return set
}

// NodeProp reads a node property as a runtime value; ok is false when
// absent (including the engine's empty-string-means-missing convention).
func (s *SnapshotGraph) NodeProp(node chickpeas.NodeID, key string) (value.Value, bool) {
	return propValue(s.g.Prop(node, key))
}

// NodePropEq compares a stored node property against v: absent equals only
// Null; present values compare with value.Equal's coercion.
func (s *SnapshotGraph) NodePropEq(node chickpeas.NodeID, key string, v value.Value) bool {
	stored, ok := s.NodeProp(node, key)
	if !ok {
		return v.IsNull()
	}
	return value.Equal(stored, v)
}

// NodePropKeys is the node's populated property keys in ascending order.
func (s *SnapshotGraph) NodePropKeys(node chickpeas.NodeID) []string {
	keys := s.g.NodePropertyKeys(node)
	sort.Strings(keys)
	return keys
}

// RelProp reads a relationship property (by CSR position) as a runtime
// value; ok is false when absent.
func (s *SnapshotGraph) RelProp(pos uint32, key string) (value.Value, bool) {
	return propValue(s.g.RelProp(pos, key))
}

// RelEndpoints is the (source, target) of the relationship at pos.
func (s *SnapshotGraph) RelEndpoints(pos uint32) (source, target chickpeas.NodeID, ok bool) {
	return s.g.RelEndpoints(pos)
}

// Neighbors iterates node's neighbors over dir, any type.
func (s *SnapshotGraph) Neighbors(node chickpeas.NodeID, dir chickpeas.Direction) iter.Seq[chickpeas.NodeID] {
	return s.g.Neighbors(node, dir)
}

// NeighborsByType iterates node's neighbors over dir restricted to types.
func (s *SnapshotGraph) NeighborsByType(node chickpeas.NodeID, dir chickpeas.Direction, types []string) iter.Seq[chickpeas.NodeID] {
	return s.g.Neighbors(node, dir, types...)
}

// NeighborsMatched iterates through a pre-resolved matcher.
func (s *SnapshotGraph) NeighborsMatched(node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) iter.Seq[chickpeas.NodeID] {
	return s.g.NeighborsMatch(node, dir, m.m)
}

// Relationships iterates traversed relationships as (neighbor, csrPos).
func (s *SnapshotGraph) Relationships(node chickpeas.NodeID, dir chickpeas.Direction, types []string) iter.Seq2[chickpeas.NodeID, uint32] {
	return func(yield func(chickpeas.NodeID, uint32) bool) {
		for r := range s.g.Rels(node, dir, types...) {
			if !yield(r.Neighbor, r.Pos) {
				return
			}
		}
	}
}

// RelationshipsMatched iterates traversed relationships as (neighbor,
// csrPos) through a pre-resolved matcher, skipping the per-call type-name
// resolution Relationships pays.
func (s *SnapshotGraph) RelationshipsMatched(node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) iter.Seq2[chickpeas.NodeID, uint32] {
	return func(yield func(chickpeas.NodeID, uint32) bool) {
		for r := range s.g.RelsMatch(node, dir, m.m) {
			if !yield(r.Neighbor, r.Pos) {
				return
			}
		}
	}
}

// AppendNeighborsMatched appends node's matching dir neighbors to dst,
// delegating to the core append accessor so the fill stays allocation-free
// across the package boundary (the iter.Seq form would escape a per-call
// yield closure to the heap).
func (s *SnapshotGraph) AppendNeighborsMatched(dst []chickpeas.NodeID, node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) []chickpeas.NodeID {
	return s.g.AppendNeighborsMatch(dst, node, dir, m.m)
}

// AppendNeighborsByType appends node's dir neighbors over types to dst,
// resolving the type match once and delegating to the core append accessor
// so the fill stays allocation-free (the iter.Seq form escapes a per-call
// yield closure across the package boundary).
func (s *SnapshotGraph) AppendNeighborsByType(dst []chickpeas.NodeID, node chickpeas.NodeID, dir chickpeas.Direction, types []string) []chickpeas.NodeID {
	return s.g.AppendNeighborsMatch(dst, node, dir, s.g.Match(types...))
}

// AppendRelationshipsMatched appends traversed relationships' neighbors
// and CSR positions to the parallel nodes/poss slices through a
// pre-resolved matcher, skipping the per-call type-name resolution on hot
// walk paths (the core iterator is a thin inlinable closure constructor,
// so the range stays allocation-free).
func (s *SnapshotGraph) AppendRelationshipsMatched(nodes []chickpeas.NodeID, poss []uint32, node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) ([]chickpeas.NodeID, []uint32) {
	for r := range s.g.RelsMatch(node, dir, m.m) {
		nodes = append(nodes, r.Neighbor)
		poss = append(poss, r.Pos)
	}
	return nodes, poss
}

// SubstringCandidates: the native backend keeps its scan-filter (no
// trigram index), matching the Rust native default.
func (s *SnapshotGraph) SubstringCandidates(label, field, needle string) (*nodeset.Set, bool) {
	return nil, false
}

// FullTextSearch serves the boolean-AND token scan from the engine's lazy
// BM25 field index.
func (s *SnapshotGraph) FullTextSearch(label, field, query string) (*nodeset.Set, bool) {
	return s.g.FullTextSearch(label, field, query), true
}

// GeoWithinRadius serves the great-circle radius scan from the engine's
// lazy k-d tree index.
func (s *SnapshotGraph) GeoWithinRadius(label, latField, lonField string, lat, lon, km float64) (*nodeset.Set, bool) {
	return s.g.GeoWithinRadius(label, latField, lonField, lat, lon, km), true
}

// GeoWithinBBox serves the bounding-box scan from the engine's geo index.
func (s *SnapshotGraph) GeoWithinBBox(label, latField, lonField string, minLat, minLon, maxLat, maxLon float64) (*nodeset.Set, bool) {
	return s.g.GeoWithinBBox(label, latField, lonField, minLat, minLon, maxLat, maxLon), true
}

// RelWeightReader hoists the weight column once per search: the returned
// reader answers 1.0 for an absent or non-numeric weight, matching the
// per-rel property fallback.
func (s *SnapshotGraph) RelWeightReader(key string) func(pos uint32) float64 {
	col, ok := s.g.RelColIndexed(key)
	if !ok {
		return func(uint32) float64 { return 1.0 }
	}
	switch col.Dtype() {
	case chickpeas.DtypeF64:
		r := col.F64()
		return func(pos uint32) float64 {
			if v, ok := r.Get(pos); ok {
				return v
			}
			return 1.0
		}
	case chickpeas.DtypeI64:
		r := col.I64()
		return func(pos uint32) float64 {
			if v, ok := r.Get(pos); ok {
				return float64(v)
			}
			return 1.0
		}
	default:
		return func(uint32) float64 { return 1.0 }
	}
}

// propValue converts an engine property read to a runtime value, folding
// the engine's absent conventions (missing, and empty/unresolvable
// strings) to not-ok.
func propValue(p chickpeas.Prop) (value.Value, bool) {
	v, ok := p.Value()
	if !ok {
		return value.Value{}, false
	}
	switch v.Kind() {
	case chickpeas.KindI64:
		i, _ := v.I64()
		return value.Int(i), true
	case chickpeas.KindF64:
		f, _ := v.F64()
		return value.Float(f), true
	case chickpeas.KindBool:
		b, _ := v.Bool()
		return value.Bool(b), true
	default:
		s, ok := p.Str()
		if !ok {
			return value.Value{}, false
		}
		return value.Str(s), true
	}
}

// engineValue converts a scalar runtime value to the engine's columnar
// Value for index lookups; ok is false for non-scalars and for strings
// absent from the interner (which can match nothing).
func engineValue(g *chickpeas.Snapshot, v value.Value) (chickpeas.Value, bool) {
	switch v.Kind() {
	case value.KindInt:
		i, _ := v.AsInt()
		return chickpeas.I64Value(i), true
	case value.KindFloat:
		f, _ := v.AsFloat()
		return chickpeas.F64Value(f), true
	case value.KindBool:
		b, _ := v.AsBool()
		return chickpeas.BoolValue(b), true
	case value.KindStr:
		s, _ := v.AsStr()
		return g.ValueFromString(s)
	default:
		return chickpeas.Value{}, false
	}
}

// compile-time check that SnapshotGraph satisfies the seam and the native
// capability.
var (
	_ Graph  = (*SnapshotGraph)(nil)
	_ Native = (*SnapshotGraph)(nil)
)
