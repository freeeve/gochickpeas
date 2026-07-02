// Behavioral tests ported from rustychickpeas-core's graph_snapshot/tests.rs
// and graph_builder/tests.rs -- the cases not already covered by the fixture
// and kernel suites. Each pins the same semantics the Rust test pins.

package chickpeas_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

// chain builds 0 -> 1 -> 2 -> 3 -> 4 over "e".
func chain(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	return buildGraph(t, 5, [][2]uint32{{0, 1}, {1, 2}, {2, 3}, {3, 4}})
}

// test_bfs_with_direction_incoming / _both / _multiple_start_nodes
func TestBFSDirectionsAndMultipleStarts(t *testing.T) {
	g := chain(t)
	all := chickpeas.MatchAll()
	// Incoming from the chain's tail walks back to the head.
	nodes, _ := g.BFS(nodeset.Of(4), chickpeas.Incoming, all, nil, nil, chickpeas.NoMaxDepth)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 1, 2, 3, 4}) {
		t.Fatalf("incoming: %v", nodes.ToSlice())
	}
	// Both from the middle reaches everything.
	nodes, _ = g.BFS(nodeset.Of(2), chickpeas.Both, all, nil, nil, chickpeas.NoMaxDepth)
	if nodes.Len() != 5 {
		t.Fatalf("both: %v", nodes.ToSlice())
	}
	// Multiple starts merge frontiers.
	nodes, _ = g.BFS(nodeset.Of(0, 4), chickpeas.Outgoing, all, nil, nil, 1)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 1, 4}) {
		t.Fatalf("multi-start: %v", nodes.ToSlice())
	}
}

// test_bidirectional_bfs_with_rel_type_filter / _with_max_depth /
// _multiple_sources_targets / _node_filter_excludes_consistently
func TestBidirectionalBFSVariants(t *testing.T) {
	// Two parallel two-hop routes 0->1->3 ("a") and 0->2->3 ("b").
	b := chickpeas.NewBuilder(8, 8)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "V")
	}
	b.AddRel(0, 1, "a")
	b.AddRel(1, 3, "a")
	b.AddRel(0, 2, "b")
	b.AddRel(2, 3, "b")
	g := b.Finalize()

	// Type filter keeps only the "a" route.
	nodes, _ := g.BidirectionalBFS(nodeset.Of(0), nodeset.Of(3), chickpeas.Outgoing,
		g.Match("a"), nil, nil, chickpeas.NoMaxDepth)
	if !nodes.Contains(1) || nodes.Contains(2) {
		t.Fatalf("type-filtered meeting: %v", nodes.ToSlice())
	}

	// Depth 0 forbids any expansion: no meeting.
	nodes, rels := g.BidirectionalBFS(nodeset.Of(0), nodeset.Of(3), chickpeas.Outgoing,
		chickpeas.MatchAll(), nil, nil, 0)
	if !nodes.IsEmpty() || !rels.IsEmpty() {
		t.Fatalf("depth-0 met: %v", nodes.ToSlice())
	}

	// Multiple sources and targets: both routes meet.
	nodes, _ = g.BidirectionalBFS(nodeset.Of(1, 2), nodeset.Of(3), chickpeas.Outgoing,
		chickpeas.MatchAll(), nil, nil, chickpeas.NoMaxDepth)
	if nodes.IsEmpty() {
		t.Fatal("multi source/target found no meeting")
	}

	// A node filter rejecting a meeting node excludes it regardless of
	// which frontier reaches it first (the order-independence fix).
	notOne := func(n chickpeas.NodeID, _ *chickpeas.Snapshot) bool { return n != 1 }
	nodes, _ = g.BidirectionalBFS(nodeset.Of(0), nodeset.Of(3), chickpeas.Outgoing,
		chickpeas.MatchAll(), notOne, nil, chickpeas.NoMaxDepth)
	if nodes.Contains(1) {
		t.Fatal("filtered node participated")
	}
	if !nodes.Contains(2) {
		t.Fatalf("unfiltered route lost: %v", nodes.ToSlice())
	}
}

// test_can_reach_all_unresolvable_types_match_everything -- pinned as the
// documented DEVIATION: in Go, unknown type names match nothing, uniformly
// with the traversal family (Rust's can_reach treats all-unknown as
// match-all).
func TestCanReachUnknownTypesDeviation(t *testing.T) {
	g := chain(t)
	if g.CanReach(0, 4, chickpeas.Outgoing, g.Match("NOPE"), chickpeas.NoMaxDepth) {
		t.Fatal("unknown type name reached through rels (Go semantics: match nothing)")
	}
	if !g.CanReach(0, 4, chickpeas.Outgoing, chickpeas.MatchAll(), chickpeas.NoMaxDepth) {
		t.Fatal("explicit MatchAll should reach")
	}
}

// common_neighbor_counts_targets_filter / _direction_and_missing_rel /
// _parallel_rels_counts_distinct_mid
func TestCommonNeighborCountsVariants(t *testing.T) {
	// Star: 0 -e-> {1,2}; 3 -e-> {1,2}; 4 -e-> {1}. Mids are 1 and 2.
	b := chickpeas.NewBuilder(8, 16)
	for i := range chickpeas.NodeID(5) {
		b.AddNodeWithID(i, "V")
	}
	for _, r := range [][2]chickpeas.NodeID{{0, 1}, {0, 2}, {3, 1}, {3, 2}, {4, 1}} {
		b.AddRel(r[0], r[1], "e")
	}
	// A parallel duplicate of 0->1 must not double-count mid 1.
	b.AddRel(0, 1, "e")
	g := b.Finalize()
	m := g.Match("e")

	// Undirected: from 0, mids {1,2} lead back to {0,3,4}.
	triples := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, m, nodeset.Of(0, 1, 2, 3, 4))
	got := map[chickpeas.NodeID]uint64{}
	for _, tr := range triples {
		got[tr.Target] = tr.Count
	}
	// (0,0): both mids loop home = 2; (0,3): both mids = 2; (0,4): mid 1 = 1.
	if got[0] != 2 || got[3] != 2 || got[4] != 1 {
		t.Fatalf("undirected counts: %v", got)
	}

	// The targets mask drops pairs outside it.
	masked := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, m, nodeset.Of(3))
	if len(masked) != 1 || masked[0].Target != 3 || masked[0].Count != 2 {
		t.Fatalf("masked: %v", masked)
	}

	// A missing rel type yields nothing.
	if got := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, g.Match("NOPE"), nodeset.Of(3)); len(got) != 0 {
		t.Fatalf("missing type counted: %v", got)
	}

	// Direction matters: Outgoing-only two-hop 0 -> mid -> t needs the
	// mid's outgoing rels; mids 1/2 have none, so no pairs.
	if got := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Outgoing, m, nodeset.Of(3)); len(got) != 0 {
		t.Fatalf("outgoing-only counted: %v", got)
	}
}

// common_neighbors_storage_agnostic_and_matches_counts
func TestCommonNeighborsMatchesCounts(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "V")
	}
	both := func(u, v chickpeas.NodeID) { b.AddRel(u, v, "k"); b.AddRel(v, u, "k") }
	both(0, 2)
	both(1, 2)
	both(0, 3)
	both(1, 3)
	g := b.Finalize()
	m := g.Match("k")
	shared := g.CommonNeighbors(0, 1, chickpeas.Both, m)
	if !slices.Equal(shared.ToSlice(), []uint32{2, 3}) {
		t.Fatalf("common neighbors: %v", shared.ToSlice())
	}
	triples := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, m, nodeset.Of(1))
	if len(triples) != 1 || triples[0].Count != uint64(shared.Len()) {
		t.Fatalf("counts disagree with the set: %v vs %d", triples, shared.Len())
	}
}

// test_nodes_with_property_probe_does_not_intern +
// test_get_nodes_with_property{_f64,_bool} on the builder
func TestBuilderNodesWithPropertyProbe(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	b.AddNodeWithID(0, "Person")
	b.AddNodeWithID(1, "Person")
	b.AddNodeWithID(2, "Bot")
	b.SetProp(0, "name", "alice")
	b.SetProp(1, "name", "alice")
	b.SetProp(2, "name", "alice")
	b.SetProp(0, "score", 1.5)
	b.SetProp(1, "active", true)

	if got := b.NodesWithProperty("Person", "name", "alice"); !slices.Equal(got, []chickpeas.NodeID{0, 1}) {
		t.Fatalf("label-scoped probe: %v", got)
	}
	if got := b.NodesWithProperty("Person", "score", 1.5); !slices.Equal(got, []chickpeas.NodeID{0}) {
		t.Fatalf("f64 probe: %v", got)
	}
	if got := b.NodesWithProperty("Person", "active", true); !slices.Equal(got, []chickpeas.NodeID{1}) {
		t.Fatalf("bool probe: %v", got)
	}
	// The probe must not intern its value or key: the finalized atom table
	// must not contain the never-staged string.
	if got := b.NodesWithProperty("Person", "name", "never-seen"); got != nil {
		t.Fatalf("unknown value matched: %v", got)
	}
	if _, ok := b.Prop(0, "never-seen"); ok {
		t.Fatal("probe interned its key")
	}
	g := b.Finalize()
	if _, ok := g.ValueFromString("never-seen"); ok {
		t.Fatal("probe leaked the value into the atom table")
	}
	// Resolve round trip for staged values.
	if v, ok := b2ResolveName(t); !ok || v != "alice" {
		t.Fatalf("resolve: %q/%v", v, ok)
	}
}

func b2ResolveName(t *testing.T) (string, bool) {
	t.Helper()
	b := chickpeas.NewBuilder(2, 0)
	b.AddNodeWithID(0, "P")
	b.SetProp(0, "name", "alice")
	v, ok := b.Prop(0, "name")
	if !ok {
		return "", false
	}
	atom, _ := v.StrID()
	return b.ResolveString(atom)
}

// test_rel_index_stays_consistent_after_lazy_build: rels added AFTER the
// lazy (u,v,type) map is built (by a SetRelProp) must still be addressable.
func TestRelIndexConsistentAfterLazyBuild(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	b.AddNodeWithID(0, "V")
	b.AddNodeWithID(1, "V")
	b.AddRel(0, 1, "e")
	if err := b.SetRelProp(0, 1, "e", "w", int64(1)); err != nil { // builds the map
		t.Fatal(err)
	}
	b.AddRel(1, 0, "e") // added after the build
	if err := b.SetRelProp(1, 0, "e", "w", int64(2)); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()
	vals := map[int64]bool{}
	for r := range g.Rels(0, chickpeas.Both) {
		vals[g.RelProp(r.Pos, "w").I64Or(-1)] = true
	}
	if !vals[1] || !vals[2] {
		t.Fatalf("rel props lost: %v", vals)
	}
}

// test_relationships_properties_survive_roundtrip: rel props read the same
// through both directions after an RCPG round trip (inToOut rebuilt).
func TestRelPropsSurviveRoundTrip(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	b.AddNodeWithID(0, "V")
	b.AddNodeWithID(1, "V")
	i0, _ := b.AddRel(0, 1, "e")
	i1, _ := b.AddRel(0, 1, "e") // parallel pair
	b.SetRelPropAt(i0, "w", int64(10))
	b.SetRelPropAt(i1, "w", int64(20))
	g := b.Finalize()

	var buf []byte
	{
		var w byteBuffer
		if err := g.WriteRCPG(&w); err != nil {
			t.Fatal(err)
		}
		buf = w
	}
	back, err := chickpeas.ReadRCPG(buf)
	if err != nil {
		t.Fatal(err)
	}
	collect := func(s *chickpeas.Snapshot, node chickpeas.NodeID, dir chickpeas.Direction) []int64 {
		var out []int64
		for r := range s.Rels(node, dir) {
			out = append(out, s.RelProp(r.Pos, "w").I64Or(-1))
		}
		slices.Sort(out)
		return out
	}
	want := []int64{10, 20}
	for _, s := range []*chickpeas.Snapshot{g, back} {
		if got := collect(s, 0, chickpeas.Outgoing); !slices.Equal(got, want) {
			t.Fatalf("outgoing: %v", got)
		}
		if got := collect(s, 1, chickpeas.Incoming); !slices.Equal(got, want) {
			t.Fatalf("incoming (parallel-pair inToOut): %v", got)
		}
	}
}

type byteBuffer []byte

func (b *byteBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// test_neighbor_counts on a fan-in shape + test_get_neighbors invalid ids.
func TestNeighborCountsAndInvalidIDs(t *testing.T) {
	g := buildGraph(t, 4, [][2]uint32{{0, 2}, {1, 2}, {0, 3}})
	counts := g.NeighborCounts([]chickpeas.NodeID{0, 1}, chickpeas.Outgoing, chickpeas.MatchAll())
	if counts[2] != 2 || counts[3] != 1 || len(counts) != 2 {
		t.Fatalf("counts: %v", counts)
	}
	// Sources outside the id space contribute nothing and never panic.
	counts = g.NeighborCounts([]chickpeas.NodeID{99}, chickpeas.Outgoing, chickpeas.MatchAll())
	if len(counts) != 0 {
		t.Fatalf("oob source counted: %v", counts)
	}
}
