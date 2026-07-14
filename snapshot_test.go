package chickpeas_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/rcpg"
)

// The conformance corpus doubles as the snapshot fixture set: known graphs
// with documented adjacency, columns, and hazards (sparse ids, parallel
// rels, NaN payloads).
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("rcpg", "testdata", "conformance", name+".rcpg"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return raw
}

func fixture(t *testing.T, name string) *chickpeas.Snapshot {
	t.Helper()
	g, err := chickpeas.ReadRCPG(fixtureBytes(t, name))
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return g
}

// TestSnapshotRCPGRoundTripByteIdentical: reading a golden file into a full
// Snapshot (maps, typed columns, rebuilt inToOut) and writing it back must
// reproduce the file exactly -- the conversion is lossless and the writer
// deterministic.
func TestSnapshotRCPGRoundTripByteIdentical(t *testing.T) {
	for _, name := range []string{"empty", "small", "sparse_ids", "all_columns", "multi_label_types", "big"} {
		t.Run(name, func(t *testing.T) {
			raw := fixtureBytes(t, name)
			g, err := chickpeas.ReadRCPG(raw)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var buf bytes.Buffer
			if err := g.WriteRCPG(&buf); err != nil {
				t.Fatalf("write: %v", err)
			}
			if !bytes.Equal(raw, buf.Bytes()) {
				t.Fatalf("round trip differs from golden bytes (got %d, want %d)",
					buf.Len(), len(raw))
			}
		})
	}
	// A topology-only rewrite of a topology-only file also matches.
	t.Run("topology_only", func(t *testing.T) {
		raw := fixtureBytes(t, "topology_only")
		g, err := chickpeas.ReadRCPG(raw)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := g.WriteRCPGWith(&buf, rcpg.TopologyOnlyWriteOptions()); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, buf.Bytes()) {
			t.Fatal("topology-only round trip differs from golden bytes")
		}
	})
}

// multi_label_types rels in insertion order (outgoing CSR position order):
// pos0 (0,0,LOOP)  pos1 (1,2,DUP)  pos2 (1,2,DUP)  pos3 (1,2,OTHER)
// pos4 (2,1,DUP); rel column "OTHER" (atom 5) holds DenseI64 [1,2,3,4,5].
func TestNeighborsAndTypeFilters(t *testing.T) {
	g := fixture(t, "multi_label_types")

	collect := func(node chickpeas.NodeID, dir chickpeas.Direction, types ...string) []uint32 {
		return slices.Collect(g.Neighbors(node, dir, types...))
	}
	cases := []struct {
		node  chickpeas.NodeID
		dir   chickpeas.Direction
		types []string
		want  []uint32
	}{
		{0, chickpeas.Outgoing, nil, []uint32{0}},                            // self-loop out
		{0, chickpeas.Incoming, nil, []uint32{0}},                            // self-loop in
		{0, chickpeas.Both, nil, []uint32{0, 0}},                             // both sides of the loop
		{1, chickpeas.Outgoing, nil, []uint32{2, 2, 2}},                      // parallel rels preserved
		{1, chickpeas.Outgoing, []string{"DUP"}, []uint32{2, 2}},             // single-type filter
		{1, chickpeas.Outgoing, []string{"OTHER"}, []uint32{2}},              //
		{1, chickpeas.Outgoing, []string{"DUP", "OTHER"}, []uint32{2, 2, 2}}, // multi-type
		{1, chickpeas.Outgoing, []string{"NOPE"}, nil},                       // unknown matches nothing
		{1, chickpeas.Incoming, nil, []uint32{2}},
		{2, chickpeas.Both, nil, []uint32{1, 1, 1, 1}}, // out then in
	}
	for _, c := range cases {
		if got := collect(c.node, c.dir, c.types...); !slices.Equal(got, c.want) {
			t.Errorf("neighbors(%d, %v, %v): got %v, want %v", c.node, c.dir, c.types, got, c.want)
		}
	}

	// Pre-resolved match agrees with the string path, and early break works.
	m := g.Match("DUP")
	got := slices.Collect(g.NeighborsMatch(1, chickpeas.Outgoing, m))
	if !slices.Equal(got, []uint32{2, 2}) {
		t.Fatalf("NeighborsMatch: got %v", got)
	}
	for range g.NeighborsMatch(1, chickpeas.Outgoing, chickpeas.MatchAll()) {
		break
	}
	if n := slices.Collect(g.NeighborsMatch(1, chickpeas.Outgoing, chickpeas.MatchNone())); len(n) != 0 {
		t.Fatal("MatchNone yielded neighbors")
	}
}

// TestRelPositionsBothDirections verifies RelRef.Pos through the rebuilt
// inToOut map: property reads through incoming traversal must see the same
// values as outgoing (the k-th-pairing hazard for parallel rels).
func TestRelPositionsBothDirections(t *testing.T) {
	g := fixture(t, "multi_label_types")
	valuesVia := func(node chickpeas.NodeID, dir chickpeas.Direction, types ...string) []int64 {
		var out []int64
		for r := range g.Rels(node, dir, types...) {
			out = append(out, g.RelProp(r.Pos, "OTHER").I64Or(-1))
		}
		slices.Sort(out)
		return out
	}
	// Outgoing positions of node 1 are pos1..pos3 -> values 2,3,4.
	if got := valuesVia(1, chickpeas.Outgoing); !slices.Equal(got, []int64{2, 3, 4}) {
		t.Fatalf("outgoing values: got %v", got)
	}
	// Incoming to node 2 are the same three rels: the parallel DUP pair must
	// map to distinct outgoing positions (2 and 3), not one twice.
	if got := valuesVia(2, chickpeas.Incoming); !slices.Equal(got, []int64{2, 3, 4}) {
		t.Fatalf("incoming values: got %v", got)
	}
	if got := valuesVia(2, chickpeas.Incoming, "DUP"); !slices.Equal(got, []int64{2, 3}) {
		t.Fatalf("incoming DUP values: got %v", got)
	}
	// The self-loop reads value 1 from either side; rel (2,1) reads 5.
	if got := valuesVia(0, chickpeas.Incoming); !slices.Equal(got, []int64{1}) {
		t.Fatalf("self-loop incoming: got %v", got)
	}
	if got := valuesVia(1, chickpeas.Incoming); !slices.Equal(got, []int64{5}) {
		t.Fatalf("node1 incoming: got %v", got)
	}

	// Direction and type are reported relative to the queried node.
	for r := range g.Rels(2, chickpeas.Incoming, "OTHER") {
		if r.Direction != chickpeas.Incoming || r.Neighbor != 1 {
			t.Fatalf("rel ref wrong: %+v", r)
		}
		if name, _ := g.ResolveString(r.Type.ID()); name != "OTHER" {
			t.Fatalf("rel type wrong: %+v", r)
		}
	}
}

func TestRelEndpoints(t *testing.T) {
	g := fixture(t, "multi_label_types")
	cases := []struct {
		pos      uint32
		src, dst chickpeas.NodeID
	}{
		{0, 0, 0}, {1, 1, 2}, {2, 1, 2}, {3, 1, 2}, {4, 2, 1},
	}
	for _, c := range cases {
		src, dst, ok := g.RelEndpoints(c.pos)
		if !ok || src != c.src || dst != c.dst {
			t.Errorf("RelEndpoints(%d): got (%d,%d,%v), want (%d,%d)", c.pos, src, dst, ok, c.src, c.dst)
		}
	}
	if _, _, ok := g.RelEndpoints(5); ok {
		t.Fatal("out-of-range position resolved")
	}
	// Empty ranges share offsets: on sparse_ids the sources are 0, 1000, 0.
	sparse := fixture(t, "sparse_ids")
	for _, c := range []struct {
		pos      uint32
		src, dst chickpeas.NodeID
	}{{0, 0, 65000}, {1, 0, 5}, {2, 1000, 5}} {
		src, dst, ok := sparse.RelEndpoints(c.pos)
		if !ok || src != c.src || dst != c.dst {
			t.Errorf("sparse RelEndpoints(%d): got (%d,%d,%v), want (%d,%d)", c.pos, src, dst, ok, c.src, c.dst)
		}
	}
}

func TestRelTypeAt(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	mk := func(label string) chickpeas.NodeID {
		n, err := b.AddNode(label)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	a, bn, c, d := mk("N"), mk("N"), mk("N"), mk("N")
	if _, err := b.AddRel(a, bn, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(c, d, "LIKES"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize("reltype")
	// Outgoing-CSR positions group by ascending source id: a's edge is pos 0,
	// c's is pos 1.
	for _, tc := range []struct {
		pos  uint32
		want string
	}{{0, "KNOWS"}, {1, "LIKES"}} {
		if name, ok := g.RelTypeAt(tc.pos); !ok || name != tc.want {
			t.Errorf("RelTypeAt(%d) = (%q, %v), want %q", tc.pos, name, ok, tc.want)
		}
	}
	// The bool is a bounds guard: an out-of-range position resolves false.
	if _, ok := g.RelTypeAt(2); ok {
		t.Fatal("RelTypeAt on an out-of-range position must report false")
	}
}

func TestSchemaIntrospection(t *testing.T) {
	g := fixture(t, "multi_label_types")
	if got := g.Labels(); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("labels: got %v", got)
	}
	if got := g.RelTypes(); !slices.Equal(got, []string{"DUP", "LOOP", "OTHER"}) {
		t.Fatalf("rel types: got %v", got)
	}
	if g.NodeCount() != 3 || g.RelCount() != 5 || g.CSRIDSpace() != 3 {
		t.Fatalf("counts: %d/%d/%d", g.NodeCount(), g.RelCount(), g.CSRIDSpace())
	}
	if g.RelTypeCount("DUP") != 3 || g.RelTypeCount("NOPE") != 0 {
		t.Fatal("rel type counts wrong")
	}
	byType := g.RelCountByType()
	want := []chickpeas.RelTypeCountEntry{{Type: "DUP", Count: 3}, {Type: "LOOP", Count: 1}, {Type: "OTHER", Count: 1}}
	if !slices.Equal(byType, want) {
		t.Fatalf("count by type: got %v", byType)
	}
	// DUP: 3 rels, sources {1,2}, targets {2,1}.
	if got := g.AvgDegree("DUP", chickpeas.Outgoing); got != 1.5 {
		t.Fatalf("avg degree out: got %v", got)
	}
	if got := g.AvgDegree("DUP", chickpeas.Both); got != 1.5 {
		t.Fatalf("avg degree both: got %v", got)
	}
	if g.AvgDegree("NOPE", chickpeas.Outgoing) != 0 {
		t.Fatal("absent type has degree")
	}
	if s, ok := g.RelTypeStats("DUP"); !ok || s.OutSources != 2 || s.InSources != 2 {
		t.Fatalf("stats: %+v/%v", s, ok)
	}

	if !g.HasLabel(0, "A") || g.HasLabel(2, "A") || g.HasLabel(0, "NOPE") {
		t.Fatal("label membership wrong")
	}
	b, ok := g.NodesWithLabel("B")
	if !ok || !slices.Equal(b.ToSlice(), []uint32{1, 2}) {
		t.Fatal("nodes with label B wrong")
	}
	if _, ok := g.NodesWithLabel("NOPE"); ok {
		t.Fatal("unknown label resolved")
	}
	dup, ok := g.RelsWithType("DUP")
	if !ok || !slices.Equal(dup.ToSlice(), []uint32{1, 2, 4}) {
		t.Fatal("rels with type DUP wrong (positions)")
	}

	if v, ok := g.Version(); ok {
		t.Fatalf("multi fixture has no version, got %q", v)
	}
	empty := fixture(t, "empty")
	if empty.NodeCount() != 0 || len(empty.Labels()) != 0 {
		t.Fatal("empty graph not empty")
	}
	if n := slices.Collect(empty.Neighbors(0, chickpeas.Both)); len(n) != 0 {
		t.Fatal("empty graph has neighbors")
	}
}

func TestSparseIDSafety(t *testing.T) {
	g := fixture(t, "sparse_ids")
	if g.CSRIDSpace() != 65001 || g.NodeCount() != 4 {
		t.Fatalf("id space %d / nodes %d", g.CSRIDSpace(), g.NodeCount())
	}
	if got := slices.Collect(g.Neighbors(0, chickpeas.Outgoing)); !slices.Equal(got, []uint32{65000, 5}) {
		t.Fatalf("node 0 out: got %v", got)
	}
	if got := slices.Collect(g.Neighbors(65000, chickpeas.Incoming)); !slices.Equal(got, []uint32{0}) {
		t.Fatalf("node 65000 in: got %v", got)
	}
	// An id inside the space but never added has empty adjacency; an id
	// outside the space is safely empty too.
	if len(slices.Collect(g.Neighbors(500, chickpeas.Both))) != 0 {
		t.Fatal("absent id has neighbors")
	}
	if len(slices.Collect(g.Neighbors(70000, chickpeas.Both))) != 0 {
		t.Fatal("out-of-space id has neighbors")
	}
	if v, ok := g.Prop(65000, "weight").I64(); !ok || v != int64(^uint64(0)>>1) {
		t.Fatalf("high-id sparse prop: got %d/%v", v, ok)
	}
	thing, ok := g.NodesWithLabel("Thing")
	if !ok || thing.Len() != 4 {
		t.Fatal("Thing label wrong")
	}
	set, ok := g.NodesWithProperty("Thing", "weight", 500)
	if !ok || !slices.Equal(set.ToSlice(), []uint32{5}) {
		t.Fatal("sparse property lookup wrong")
	}
}

func TestSingleStepHelpers(t *testing.T) {
	g := fixture(t, "small") // 0 <-> 1 via KNOWS; names alice/bob
	if n, ok := g.FirstNeighbor(0, chickpeas.Outgoing, "KNOWS"); !ok || n != 1 {
		t.Fatal("first neighbor wrong")
	}
	if _, ok := g.FirstNeighbor(0, chickpeas.Outgoing, "NOPE"); ok {
		t.Fatal("unknown type yielded a neighbor")
	}
	out := chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "KNOWS"}
	if n, ok := g.Follow(0, out, out); !ok || n != 0 {
		t.Fatal("two KNOWS hops should return home")
	}
	if _, ok := g.Follow(0, out, chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "NOPE"}); ok {
		t.Fatal("broken chain resolved")
	}
	if !g.HasRel(0, chickpeas.Outgoing, "KNOWS") || g.HasRel(0, chickpeas.Outgoing, "NOPE") {
		t.Fatal("HasRel wrong")
	}
	if !g.HasNeighborWithProperty(0, chickpeas.Outgoing, "name", "bob", "KNOWS") {
		t.Fatal("neighbor bob not found")
	}
	if g.HasNeighborWithProperty(0, chickpeas.Outgoing, "name", "alice", "KNOWS") {
		t.Fatal("neighbor alice should not match")
	}
	if g.HasNeighborWithProperty(0, chickpeas.Outgoing, "name", "never-interned") {
		t.Fatal("unknown value matched")
	}

	people, _ := g.NodesWithLabel("Person")
	got := slices.Collect(g.NeighborsInSet(0, chickpeas.Both, people, "KNOWS"))
	if !slices.Equal(got, []uint32{1, 1}) {
		t.Fatalf("neighbors in set: got %v", got)
	}

	sum := chickpeas.ParNeighborFold(g, 0, chickpeas.Both, g.Match("KNOWS"),
		func() int { return 0 },
		func(acc int, n chickpeas.NodeID) int { return acc + int(n) },
		func(a, b int) int { return a + b })
	if sum != 2 {
		t.Fatalf("par neighbor fold: got %d", sum)
	}
}
