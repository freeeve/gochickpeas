package chickpeas_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

// threadFixture: a reply forest with creators, the RootsVia/FoldVia shape.
//
//	posts 0, 1 (roots); replies 2->0, 3->2, 4->1 via REPLY_OF (outgoing)
//	creators via CREATOR: 0->10, 1->11, 2->11, 3->10, 4->10
func threadFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	for i := range chickpeas.NodeID(5) {
		b.AddNodeWithID(i, "Message")
	}
	b.AddNodeWithID(10, "Person")
	b.AddNodeWithID(11, "Person")
	for _, r := range [][2]chickpeas.NodeID{{2, 0}, {3, 2}, {4, 1}} {
		b.AddRel(r[0], r[1], "REPLY_OF")
	}
	for _, r := range [][2]chickpeas.NodeID{{0, 10}, {1, 11}, {2, 11}, {3, 10}, {4, 10}} {
		b.AddRel(r[0], r[1], "CREATOR")
	}
	// A per-message day for CoDistinct.
	for msg, day := range map[chickpeas.NodeID]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 1} {
		b.SetProp(msg, "day", day)
	}
	return b.Finalize()
}

func TestRootsViaAndNeighborVia(t *testing.T) {
	g := threadFixture(t)
	replyOf, _ := g.RelType("REPLY_OF")
	roots := g.RootsVia(replyOf, chickpeas.Outgoing)
	for node, want := range map[chickpeas.NodeID]chickpeas.NodeID{
		0: 0, 1: 1, 2: 0, 3: 0, 4: 1, 10: 10, 11: 11,
	} {
		if roots[node] != want {
			t.Fatalf("root[%d]: got %d, want %d", node, roots[node], want)
		}
	}
	// Cached: the second call returns the same array.
	again := g.RootsVia(replyOf, chickpeas.Outgoing)
	if &again[0] != &roots[0] {
		t.Fatal("roots array not cached")
	}
	if g.RootVia(3, replyOf, chickpeas.Outgoing) != 0 {
		t.Fatal("RootVia wrong")
	}

	creator, _ := g.RelType("CREATOR")
	via := g.NeighborVia(creator, chickpeas.Outgoing)
	if via[4] != 10 || via[2] != 11 {
		t.Fatalf("neighbor via: %v", via[:5])
	}
	if via[10] != chickpeas.NoNeighbor {
		t.Fatal("person has no CREATOR neighbor")
	}
}

func TestRootsViaCycleTerminates(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	b.AddNodeWithID(0, "N")
	b.AddNodeWithID(1, "N")
	b.AddRel(0, 1, "NEXT")
	b.AddRel(1, 0, "NEXT") // a malformed 2-cycle
	g := b.Finalize()
	next, _ := g.RelType("NEXT")
	roots := g.RootsVia(next, chickpeas.Outgoing)
	if len(roots) != 2 {
		t.Fatalf("roots: %v", roots)
	}
	// Deterministic: whatever terminal the cap picks, both calls agree.
	if !slices.Equal(roots, g.RootsVia(next, chickpeas.Outgoing)) {
		t.Fatal("cycle resolution not deterministic")
	}
}

func TestFoldVia(t *testing.T) {
	g := threadFixture(t)
	creator, _ := g.RelType("CREATOR")
	toCreator := g.NeighborVia(creator, chickpeas.Outgoing)
	// Fold REPLY_OF through creators: 2->0 = (11,10), 3->2 = (10,11),
	// 4->1 = (10,11). Unordered pairs: (10,11) x3.
	pairs := g.FoldVia(g.Match("REPLY_OF"), chickpeas.Outgoing, toCreator)
	if len(pairs) != 1 || pairs[chickpeas.NodePair{Lo: 10, Hi: 11}] != 3 {
		t.Fatalf("fold: %v", pairs)
	}
	// Unknown type folds to empty.
	if got := g.FoldVia(g.Match("NOPE"), chickpeas.Outgoing, toCreator); len(got) != 0 {
		t.Fatalf("unknown type folded: %v", got)
	}
}

func TestCoOccurring(t *testing.T) {
	// Bipartite person -ATTENDS-> event.
	b := chickpeas.NewBuilder(16, 16)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "Person")
	}
	b.AddNodeWithID(10, "Event")
	b.AddNodeWithID(11, "Event")
	attend := func(p, e chickpeas.NodeID) { b.AddRel(p, e, "ATTENDS") }
	attend(0, 10)
	attend(0, 11)
	attend(1, 10)
	attend(1, 11)
	attend(2, 11)
	b.SetProp(10, "day", int64(1))
	b.SetProp(11, "day", int64(1)) // same day: distinct-day weight collapses
	g := b.Finalize()

	counts := g.CoOccurring(0, g.Match("ATTENDS"), chickpeas.Outgoing, chickpeas.CoCount())
	if counts[1] != 2 || counts[2] != 1 || len(counts) != 2 {
		t.Fatalf("co count: %v", counts)
	}
	days := g.CoOccurring(0, g.Match("ATTENDS"), chickpeas.Outgoing, chickpeas.CoDistinct("day"))
	if days[1] != 1 || days[2] != 1 {
		t.Fatalf("co distinct: %v", days)
	}
	if got := g.CoOccurring(0, g.Match("ATTENDS"), chickpeas.Outgoing, chickpeas.CoDistinct("nope")); len(got) != 0 {
		t.Fatal("unknown key co-occurred")
	}
}

func TestNeighborAndCommonNeighborCounts(t *testing.T) {
	g := threadFixture(t)
	m := g.Match("CREATOR")
	counts := g.NeighborCounts([]chickpeas.NodeID{0, 1, 2, 3, 4}, chickpeas.Outgoing, m)
	if counts[10] != 3 || counts[11] != 2 {
		t.Fatalf("neighbor counts: %v", counts)
	}

	// Common neighbors of the two persons via incoming CREATOR: none share
	// a message. Messages 0 and 3 share creator 10.
	common := g.CommonNeighbors(0, 3, chickpeas.Outgoing, m)
	if !slices.Equal(common.ToSlice(), []uint32{10}) {
		t.Fatalf("common: %v", common.ToSlice())
	}

	// Masked A^2 over the undirected KNOWS-style shape: use CREATOR Both.
	targets := nodeset.Of(0, 1, 2, 3, 4)
	triples := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, m, targets)
	// From message 0 through creator 10: messages 0, 3, 4 -> pairs
	// (0,0,1)(0,3,1)(0,4,1).
	want := map[chickpeas.NodeID]uint64{0: 1, 3: 1, 4: 1}
	if len(triples) != len(want) {
		t.Fatalf("triples: %v", triples)
	}
	for _, tr := range triples {
		if tr.Source != 0 || want[tr.Target] != tr.Count {
			t.Fatalf("triple: %+v", tr)
		}
	}
}

// TestCommonNeighborCountsDedup pins the distinct-mid semantics on a
// double-stored (bidirectional) rel traversed Both: each mid counts once.
func TestCommonNeighborCountsDedup(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	for i := range chickpeas.NodeID(3) {
		b.AddNodeWithID(i, "P")
	}
	// KNOWS stored both ways (the LDBC convention).
	both := func(u, v chickpeas.NodeID) {
		b.AddRel(u, v, "KNOWS")
		b.AddRel(v, u, "KNOWS")
	}
	both(0, 1) // mid
	both(1, 2)
	g := b.Finalize()
	triples := g.CommonNeighborCounts([]chickpeas.NodeID{0}, chickpeas.Both, g.Match("KNOWS"), nodeset.Of(2))
	if len(triples) != 1 || triples[0].Count != 1 {
		t.Fatalf("double-stored dedup: %v", triples)
	}
}

func TestNeighborGroups(t *testing.T) {
	g := threadFixture(t)
	// Group each message's REPLY_OF-incoming replies by the reply's
	// creator: message 0 has replies {2 (creator 11)}, message 2 has {3
	// (creator 10)}, message 1 has {4 (creator 10)}.
	sources := []chickpeas.NodeID{0, 1, 2, 3}
	sizes := g.NeighborGroups(sources, g.Match("REPLY_OF"), chickpeas.Incoming).
		Project(chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "CREATOR"}).
		Sizes()
	want := []uint32{1, 1, 1, 0}
	for i, s := range sizes {
		if s.Source != sources[i] || s.Size != want[i] {
			t.Fatalf("sizes[%d]: %+v want size %d", i, s, want[i])
		}
	}
	top := g.NeighborGroups(sources, g.Match("REPLY_OF"), chickpeas.Incoming).
		Project(chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "CREATOR"}).
		TopBySize(2, "")
	if len(top) != 2 || top[0].Size != 1 || top[0].Source != 0 {
		t.Fatalf("top: %v", top)
	}
	// An unknown projection type yields all zeros.
	zeros := g.NeighborGroups(sources, g.Match("REPLY_OF"), chickpeas.Incoming).
		Project(chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "NOPE"}).
		Sizes()
	for _, s := range zeros {
		if s.Size != 0 {
			t.Fatalf("unknown projection produced %+v", s)
		}
	}
}
