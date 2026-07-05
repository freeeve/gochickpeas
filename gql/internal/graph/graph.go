// Package graph is the GQL engine's seam to the graph store: the portable
// read surface the planner and executor bind to (port of the Rust
// CypherGraph trait's data methods). Consumer-side interface per the
// DESIGN.md seam contract; *chickpeas.Snapshot (via New) is the one
// implementation. Deviations from the Rust trait, both deliberate: executor
// hooks (eval compilation, CALL/shortest-path offload) live with the
// executor behind the Native capability instead of on this interface, and
// inline-property parameters are resolved to runtime values by the caller
// at matcher-compile time instead of through thread-local scopes.
package graph

import (
	"iter"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// Graph is the minimal read surface the interpreter and the generic
// executor need. Methods returning *nodeset.Set return sets shared with
// the store's lazy indexes: callers must never mutate them (Clone first);
// a nil set reads as empty.
type Graph interface {
	// NodeCount is the number of distinct node ids -- a planner cardinality
	// statistic, NOT the id-space size: under sparse ids it is smaller than
	// IDSpace. Size dense node-indexed scratch by IDSpace, never this.
	NodeCount() uint32
	// IDSpace is one past the largest id any node can take, so a dense
	// 0..IDSpace() array indexed by node id never goes out of bounds.
	IDSpace() uint32
	// AvgDegree is the average degree over relType in dir -- a planner
	// statistic used to break anchor ties; 0 when the type is unknown.
	AvgDegree(relType string, dir chickpeas.Direction) float64

	// HasLabel reports whether node carries label.
	HasLabel(node chickpeas.NodeID, label string) bool
	// NodesWithLabel is the label scan's id set (shared; nil when the label
	// is unknown).
	NodesWithLabel(label string) *nodeset.Set
	// LabelCardinality is the number of nodes carrying label, from the
	// label-index cardinality without materializing ids.
	LabelCardinality(label string) uint64
	// NodesWithProperty is the selectivity anchor behind {key: value}
	// patterns: the nodes of label whose key equals v, from the inverted
	// property index (shared set; nil when nothing matches or v is not an
	// indexable scalar).
	NodesWithProperty(label, key string, v value.Value) *nodeset.Set

	// NodeProp reads a node's property as a runtime value (strings
	// resolved); ok is false when absent.
	NodeProp(node chickpeas.NodeID, key string) (value.Value, bool)
	// NodePropEq reports whether node's key property equals v. An absent
	// property equals only a Null v (matching pattern-inline semantics);
	// numerics coerce exactly like value.Compare.
	NodePropEq(node chickpeas.NodeID, key string, v value.Value) bool
	// NodePropKeys is the property keys node carries a value for, in
	// ascending key order (behind keys(n) / the n{.*} map projection).
	NodePropKeys(node chickpeas.NodeID) []string
	// RelProp reads a relationship's property (by outgoing-CSR position)
	// as a runtime value; ok is false when absent.
	RelProp(pos uint32, key string) (value.Value, bool)
	// RelEndpoints is the (source, target) of the relationship at CSR
	// position pos, backing startNode(r)/endNode(r).
	RelEndpoints(pos uint32) (source, target chickpeas.NodeID, ok bool)

	// Neighbors iterates node's neighbors over dir, any relationship type.
	Neighbors(node chickpeas.NodeID, dir chickpeas.Direction) iter.Seq[chickpeas.NodeID]
	// NeighborsByType iterates node's neighbors over dir restricted to
	// types (empty types match every type).
	NeighborsByType(node chickpeas.NodeID, dir chickpeas.Direction, types []string) iter.Seq[chickpeas.NodeID]
	// NeighborsMatched is NeighborsByType through a pre-resolved matcher,
	// skipping the per-call name resolution on hot paths.
	NeighborsMatched(node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) iter.Seq[chickpeas.NodeID]
	// Relationships iterates each traversed relationship as (neighbor,
	// csrPos); the position binds a rel variable and reads rel properties.
	Relationships(node chickpeas.NodeID, dir chickpeas.Direction, types []string) iter.Seq2[chickpeas.NodeID, uint32]

	// AppendNeighborsMatched appends node's dir neighbors passing m to dst
	// and returns the extended slice -- the batch form of NeighborsMatched
	// for the executor's hot loops: an interface-returned iter.Seq cannot
	// devirtualize, so ranging it heap-allocates its closures per call,
	// while the batch form fills a caller-pooled buffer allocation-free.
	AppendNeighborsMatched(dst []chickpeas.NodeID, node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) []chickpeas.NodeID
	// AppendNeighborsByType is AppendNeighborsMatched with per-call name
	// resolution (empty types match every type).
	AppendNeighborsByType(dst []chickpeas.NodeID, node chickpeas.NodeID, dir chickpeas.Direction, types []string) []chickpeas.NodeID
	// AppendRelationshipsMatched appends each traversed relationship's
	// neighbor and CSR position to the parallel nodes/poss slices through a
	// pre-resolved matcher (same devirtualization rationale as
	// AppendNeighborsMatched).
	AppendRelationshipsMatched(nodes []chickpeas.NodeID, poss []uint32, node chickpeas.NodeID, dir chickpeas.Direction, m *RelMatcher) ([]chickpeas.NodeID, []uint32)

	// CompileNodeMatcher pre-resolves a node pattern's labels and inline
	// {key: value} properties (params already resolved to values by the
	// caller) into a reusable matcher, once per operator.
	CompileNodeMatcher(labels []string, props []PropSpec) *NodeMatcher
	// NodeMatcherAccepts reports whether node satisfies a compiled matcher
	// -- every label present and every inline property equal, identical to
	// HasLabel && NodePropEq over the same inputs.
	NodeMatcherAccepts(m *NodeMatcher, node chickpeas.NodeID) bool
	// CompileRelMatcher pre-resolves an expand's relationship-type names,
	// once per operator: empty types match all; unknown names resolve to
	// no match.
	CompileRelMatcher(types []string) *RelMatcher

	// SubstringCandidates is a candidate superset for a STARTS WITH /
	// ENDS WITH / CONTAINS anchor scan; ok=false means no index can help
	// and the executor falls back to the label scan. Never false-negative.
	SubstringCandidates(label, field, needle string) (*nodeset.Set, bool)
	// FullTextSearch is the boolean-AND full-text index scan behind CALL
	// fts.search; ok=false means no full-text support.
	FullTextSearch(label, field, query string) (*nodeset.Set, bool)
	// GeoWithinRadius is the great-circle radius scan behind CALL
	// geo.withinRadius; ok=false means no geo support.
	GeoWithinRadius(label, latField, lonField string, lat, lon, km float64) (*nodeset.Set, bool)
	// GeoWithinBBox is the bounding-box scan behind CALL geo.withinBBox
	// (a box with minLon > maxLon crosses the antimeridian).
	GeoWithinBBox(label, latField, lonField string, minLat, minLon, maxLat, maxLon float64) (*nodeset.Set, bool)
	// RelWeightReader is a hoisted relationship-weight reader for weighted
	// shortest paths: reads the key weight by CSR position in O(1),
	// defaulting to 1.0 when absent or non-numeric.
	RelWeightReader(key string) func(pos uint32) float64
}

// PropSpec is one inline {key: value} predicate of a node pattern, with any
// parameter already resolved to its runtime value. A Null value matches
// only nodes missing the property.
type PropSpec struct {
	Key string
	Val value.Value
}

// Native is the capability a backend asserts to expose the full engine
// Snapshot for kernel offload (CALL procedures, shortest-path kernels, the
// columnar compiled eval path). The executor type-asserts Graph to Native;
// a backend without it falls back to the portable interpreter paths.
type Native interface {
	Snapshot() *chickpeas.Snapshot
}
