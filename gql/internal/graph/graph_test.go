// Adapter and matcher tests against a built engine Snapshot: the seam's
// property conversion, absent-vs-null semantics, matcher pre-resolution,
// and traversal forwarding.
package graph

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// buildGraph is a small fixture: three Persons and a City with scalar
// props of every kind, KNOWS and LIVES_IN rels, one weighted rel.
func buildGraph(t *testing.T) *SnapshotGraph {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	alice, _ := b.AddNode("Person")
	bob, _ := b.AddNode("Person")
	carol, _ := b.AddNode("Person")
	city, _ := b.AddNode("City")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(b.SetProp(alice, "name", "Alice"))
	must(b.SetProp(alice, "age", int64(30)))
	must(b.SetProp(alice, "score", 1.5))
	must(b.SetProp(alice, "active", true))
	must(b.SetProp(bob, "name", "Bob"))
	must(b.SetProp(bob, "age", int64(40)))
	must(b.SetProp(carol, "name", "Carol"))
	must(b.SetProp(city, "name", "Springfield"))
	_, err := b.AddRel(alice, bob, "KNOWS")
	must(err)
	_, err = b.AddRel(bob, carol, "KNOWS")
	must(err)
	_, err = b.AddRel(alice, city, "LIVES_IN")
	must(err)
	must(b.SetRelProp(alice, bob, "KNOWS", "weight", 2.5))
	return New(b.Finalize())
}

func TestNodePropKindsAndAbsent(t *testing.T) {
	s := buildGraph(t)
	if v, ok := s.NodeProp(0, "name"); !ok || !value.Equal(v, value.Str("Alice")) {
		t.Fatalf("name = %v, %v", v, ok)
	}
	if v, ok := s.NodeProp(0, "age"); !ok || !value.Equal(v, value.Int(30)) {
		t.Fatalf("age = %v, %v", v, ok)
	}
	if v, ok := s.NodeProp(0, "score"); !ok || !value.Equal(v, value.Float(1.5)) {
		t.Fatalf("score = %v, %v", v, ok)
	}
	if v, ok := s.NodeProp(0, "active"); !ok || !value.Equal(v, value.Bool(true)) {
		t.Fatalf("active = %v, %v", v, ok)
	}
	if _, ok := s.NodeProp(2, "age"); ok {
		t.Fatal("carol has no age")
	}
	if _, ok := s.NodeProp(0, "nosuchkey"); ok {
		t.Fatal("unknown key is absent")
	}
}

func TestNodePropEqCoercionAndNull(t *testing.T) {
	s := buildGraph(t)
	if !s.NodePropEq(0, "age", value.Int(30)) {
		t.Fatal("age = 30")
	}
	// Numerics coerce through float64, both directions.
	if !s.NodePropEq(0, "age", value.Float(30.0)) {
		t.Fatal("age = 30.0 coerces")
	}
	if !s.NodePropEq(0, "score", value.Float(1.5)) || s.NodePropEq(0, "score", value.Int(1)) {
		t.Fatal("score float compare")
	}
	if s.NodePropEq(0, "age", value.Int(31)) {
		t.Fatal("age <> 31")
	}
	// An absent property equals only Null.
	if !s.NodePropEq(2, "age", value.Null()) {
		t.Fatal("absent = null")
	}
	if s.NodePropEq(0, "age", value.Null()) {
		t.Fatal("present <> null")
	}
	if !s.NodePropEq(0, "nosuchkey", value.Null()) {
		t.Fatal("unknown key = null")
	}
}

func TestNodePropKeysSorted(t *testing.T) {
	s := buildGraph(t)
	keys := s.NodePropKeys(0)
	want := []string{"active", "age", "name", "score"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func TestRelPropAndEndpoints(t *testing.T) {
	s := buildGraph(t)
	// Find alice's KNOWS rel position via Relationships.
	var pos uint32
	var neighbor chickpeas.NodeID
	found := false
	for n, p := range s.Relationships(0, chickpeas.Outgoing, []string{"KNOWS"}) {
		neighbor, pos, found = n, p, true
	}
	if !found || neighbor != 1 {
		t.Fatalf("alice-KNOWS->bob rel not found: %v %v", neighbor, found)
	}
	if v, ok := s.RelProp(pos, "weight"); !ok || !value.Equal(v, value.Float(2.5)) {
		t.Fatalf("weight = %v, %v", v, ok)
	}
	src, dst, ok := s.RelEndpoints(pos)
	if !ok || src != 0 || dst != 1 {
		t.Fatalf("endpoints = (%d, %d, %v)", src, dst, ok)
	}
	if w := s.RelWeightReader("weight")(pos); w != 2.5 {
		t.Fatalf("hoisted weight = %v", w)
	}
	// Absent weight key: constant 1.0 fallback.
	if w := s.RelWeightReader("nosuchweight")(pos); w != 1.0 {
		t.Fatalf("absent weight = %v, want 1.0", w)
	}
}

func TestTraversalForwarding(t *testing.T) {
	s := buildGraph(t)
	collect := func(seq func(func(chickpeas.NodeID) bool)) []chickpeas.NodeID {
		var out []chickpeas.NodeID
		for n := range seq {
			out = append(out, n)
		}
		return out
	}
	if got := collect(s.Neighbors(0, chickpeas.Outgoing)); len(got) != 2 {
		t.Fatalf("alice out-neighbors = %v", got)
	}
	if got := collect(s.NeighborsByType(0, chickpeas.Outgoing, []string{"KNOWS"})); !slices.Equal(got, []chickpeas.NodeID{1}) {
		t.Fatalf("alice KNOWS = %v", got)
	}
	// Empty types match every type.
	if got := collect(s.NeighborsByType(0, chickpeas.Outgoing, nil)); len(got) != 2 {
		t.Fatalf("empty types = %v", got)
	}
	// Unknown type matches nothing.
	if got := collect(s.NeighborsByType(0, chickpeas.Outgoing, []string{"NOPE"})); len(got) != 0 {
		t.Fatalf("unknown type = %v", got)
	}
	// Matcher pre-resolution agrees with the by-name path.
	m := s.CompileRelMatcher([]string{"KNOWS"})
	if got := collect(s.NeighborsMatched(0, chickpeas.Outgoing, m)); !slices.Equal(got, []chickpeas.NodeID{1}) {
		t.Fatalf("matched KNOWS = %v", got)
	}
	all := s.CompileRelMatcher(nil)
	if got := collect(s.NeighborsMatched(0, chickpeas.Outgoing, all)); len(got) != 2 {
		t.Fatalf("matched all = %v", got)
	}
	none := s.CompileRelMatcher([]string{"NOPE"})
	if got := collect(s.NeighborsMatched(0, chickpeas.Outgoing, none)); len(got) != 0 {
		t.Fatalf("matched none = %v", got)
	}
	if got := collect(s.Neighbors(1, chickpeas.Incoming)); !slices.Equal(got, []chickpeas.NodeID{0}) {
		t.Fatalf("bob in-neighbors = %v", got)
	}
}

func TestLabelIndexAndCardinality(t *testing.T) {
	s := buildGraph(t)
	set := s.NodesWithLabel("Person")
	if set == nil || set.Len() != 3 {
		t.Fatalf("Person set = %v", set)
	}
	if s.NodesWithLabel("Nope") != nil {
		t.Fatal("unknown label is nil")
	}
	if s.LabelCardinality("Person") != 3 || s.LabelCardinality("City") != 1 || s.LabelCardinality("Nope") != 0 {
		t.Fatal("label cardinalities")
	}
	if !s.HasLabel(0, "Person") || s.HasLabel(0, "City") {
		t.Fatal("HasLabel")
	}
}

func TestNodesWithPropertyDispatch(t *testing.T) {
	s := buildGraph(t)
	set := s.NodesWithProperty("Person", "name", value.Str("Alice"))
	if set == nil || set.Len() != 1 || !set.Contains(0) {
		t.Fatalf("name=Alice set = %v", set)
	}
	set = s.NodesWithProperty("Person", "age", value.Int(40))
	if set == nil || !set.Contains(1) {
		t.Fatalf("age=40 set = %v", set)
	}
	// A string absent from the interner matches nothing.
	if s.NodesWithProperty("Person", "name", value.Str("Zelda")) != nil {
		t.Fatal("uninterned string matches nothing")
	}
	// Non-scalar anchors match nothing.
	if s.NodesWithProperty("Person", "name", value.List(nil)) != nil {
		t.Fatal("non-scalar matches nothing")
	}
}

func TestNodeMatcher(t *testing.T) {
	s := buildGraph(t)
	// Label + interned string + coercing numeric.
	m := s.CompileNodeMatcher([]string{"Person"}, []PropSpec{
		{Key: "name", Val: value.Str("Alice")},
		{Key: "age", Val: value.Float(30.0)},
	})
	if !s.NodeMatcherAccepts(m, 0) {
		t.Fatal("alice matches")
	}
	if s.NodeMatcherAccepts(m, 1) {
		t.Fatal("bob has the wrong name")
	}
	// Unknown label rejects everything.
	m = s.CompileNodeMatcher([]string{"Nope"}, nil)
	for n := range chickpeas.NodeID(4) {
		if s.NodeMatcherAccepts(m, n) {
			t.Fatal("unknown label rejects all")
		}
	}
	// A string absent from the interner matches nothing.
	m = s.CompileNodeMatcher(nil, []PropSpec{{Key: "name", Val: value.Str("Zelda")}})
	if s.NodeMatcherAccepts(m, 0) {
		t.Fatal("uninterned string matches nothing")
	}
	// An unknown key matches only a Null value.
	m = s.CompileNodeMatcher(nil, []PropSpec{{Key: "nosuchkey", Val: value.Null()}})
	if !s.NodeMatcherAccepts(m, 0) {
		t.Fatal("missing column + null matches")
	}
	m = s.CompileNodeMatcher(nil, []PropSpec{{Key: "nosuchkey", Val: value.Int(1)}})
	if s.NodeMatcherAccepts(m, 0) {
		t.Fatal("missing column + non-null rejects")
	}
	// A known column with an absent value matches only Null.
	m = s.CompileNodeMatcher(nil, []PropSpec{{Key: "age", Val: value.Null()}})
	if !s.NodeMatcherAccepts(m, 2) || s.NodeMatcherAccepts(m, 0) {
		t.Fatal("null matches absent, not present")
	}
	// Bool predicate.
	m = s.CompileNodeMatcher(nil, []PropSpec{{Key: "active", Val: value.Bool(true)}})
	if !s.NodeMatcherAccepts(m, 0) || s.NodeMatcherAccepts(m, 1) {
		t.Fatal("bool predicate")
	}
	// Matcher agrees with HasLabel + NodePropEq (the trait's contract).
	m = s.CompileNodeMatcher([]string{"Person"}, []PropSpec{{Key: "age", Val: value.Int(40)}})
	for n := range chickpeas.NodeID(4) {
		want := s.HasLabel(n, "Person") && s.NodePropEq(n, "age", value.Int(40))
		if got := s.NodeMatcherAccepts(m, n); got != want {
			t.Fatalf("matcher disagrees with HasLabel+NodePropEq at node %d: %v != %v", n, got, want)
		}
	}
}

func TestIDSpaceExceedsNodeCountUnderSparseIDs(t *testing.T) {
	b := chickpeas.NewBuilder(16, 0)
	if _, err := b.AddNodeWithID(10, "Person"); err != nil {
		t.Fatal(err)
	}
	s := New(b.Finalize())
	if s.NodeCount() != 1 {
		t.Fatalf("node count = %d", s.NodeCount())
	}
	if s.IDSpace() < 11 {
		t.Fatalf("id space = %d, want >= 11", s.IDSpace())
	}
}

func TestCapabilityHooks(t *testing.T) {
	s := buildGraph(t)
	if _, ok := s.SubstringCandidates("Person", "name", "lic"); ok {
		t.Fatal("native keeps its scan-filter (no trigram index)")
	}
	set, ok := s.FullTextSearch("Person", "name", "alice")
	if !ok || set == nil || !set.Contains(0) {
		t.Fatalf("fts alice = %v, %v", set, ok)
	}
	var g Graph = s
	if n, ok := g.(Native); !ok || n.Snapshot() == nil {
		t.Fatal("SnapshotGraph asserts the Native capability")
	}
}

func TestStatisticsAndGeoHooks(t *testing.T) {
	s := buildGraph(t)
	if s.AvgDegree("KNOWS", chickpeas.Outgoing) <= 0 {
		t.Fatal("KNOWS avg degree > 0")
	}
	if s.AvgDegree("NOPE", chickpeas.Outgoing) != 0 {
		t.Fatal("unknown type has zero degree")
	}
	b := chickpeas.NewBuilder(4, 0)
	place, _ := b.AddNode("Place")
	if err := b.SetProp(place, "lat", 48.8566); err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(place, "lon", 2.3522); err != nil {
		t.Fatal(err)
	}
	g := New(b.Finalize())
	set, ok := g.GeoWithinRadius("Place", "lat", "lon", 48.85, 2.35, 5)
	if !ok || set == nil || !set.Contains(uint32(place)) {
		t.Fatalf("radius hit = %v, %v", set, ok)
	}
	set, ok = g.GeoWithinBBox("Place", "lat", "lon", 48, 2, 49, 3)
	if !ok || set == nil || !set.Contains(uint32(place)) {
		t.Fatalf("bbox hit = %v, %v", set, ok)
	}
	set, _ = g.GeoWithinBBox("Place", "lat", "lon", 10, 2, 11, 3)
	if set != nil && set.Contains(uint32(place)) {
		t.Fatal("bbox miss")
	}
}
