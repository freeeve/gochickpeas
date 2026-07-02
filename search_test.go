package chickpeas_test

import (
	"math"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

// weightedFixture: 0-1-2-3 chain plus a 0-3 shortcut, weights as "w".
//
//	0 -1-> 1 -2-> 2 -3-> 3      (path cost 6)
//	0 ---------10--------> 3    (direct cost 10)
func weightedFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "N")
	}
	add := func(u, v chickpeas.NodeID, w float64) {
		idx, err := b.AddRel(u, v, "R")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(idx, "w", w); err != nil {
			t.Fatal(err)
		}
	}
	add(0, 1, 1)
	add(1, 2, 2)
	add(2, 3, 3)
	add(0, 3, 10)
	return b.Finalize()
}

func weightOf(g *chickpeas.Snapshot) chickpeas.WeightFn {
	col, _ := g.RelCol("w")
	f64s := col.F64()
	return func(_ chickpeas.NodeID, rel chickpeas.RelRef) float64 {
		w, _ := f64s.Get(rel.Pos)
		return w
	}
}

func TestDijkstra(t *testing.T) {
	g := weightedFixture(t)
	sp := g.Dijkstra(0, chickpeas.Outgoing, chickpeas.MatchAll(), weightOf(g))
	for node, want := range map[chickpeas.NodeID]float64{0: 0, 1: 1, 2: 3, 3: 6} {
		if d, ok := sp.Distance(node); !ok || d != want {
			t.Fatalf("distance(%d): got %v/%v, want %v", node, d, ok, want)
		}
	}
	if path, ok := sp.PathTo(3); !ok || !slices.Equal(path, []uint32{0, 1, 2, 3}) {
		t.Fatalf("path to 3: %v/%v", path, ok)
	}
	// Incoming from 3 mirrors the graph.
	back := g.Dijkstra(3, chickpeas.Incoming, chickpeas.MatchAll(), weightOf(g))
	if d, _ := back.Distance(0); d != 6 {
		t.Fatalf("reverse distance: %v", d)
	}
	// Unreachable under an unknown type.
	none := g.Dijkstra(0, chickpeas.Outgoing, chickpeas.MatchNone(), weightOf(g))
	if none.Reached(1) {
		t.Fatal("reached through no rels")
	}
	if _, ok := none.PathTo(1); ok {
		t.Fatal("path through no rels")
	}
	// Early-exit variant settles the target with the same distance.
	to := g.DijkstraTo(0, 3, chickpeas.Outgoing, chickpeas.MatchAll(), weightOf(g))
	if d, ok := to.Distance(3); !ok || d != 6 {
		t.Fatalf("DijkstraTo: %v/%v", d, ok)
	}
}

// TestWeightedShortestPathMatchesDijkstra is a differential test on a
// deterministic pseudo-random graph: the bidirectional point-to-point cost
// must equal the single-source distance for every pair sampled.
func TestWeightedShortestPathMatchesDijkstra(t *testing.T) {
	const n = 60
	b := chickpeas.NewBuilder(n, 256)
	for i := range chickpeas.NodeID(n) {
		b.AddNodeWithID(i, "N")
	}
	seed := uint64(0xC0FFEE)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for range 200 {
		u := chickpeas.NodeID(next() % n)
		v := chickpeas.NodeID(next() % n)
		idx, _ := b.AddRel(u, v, "R")
		b.SetRelPropAt(idx, "w", float64(1+next()%9))
	}
	g := b.Finalize()
	w := weightOf(g)
	// Both directions of the undirected view keep w symmetric.
	for src := chickpeas.NodeID(0); src < 12; src++ {
		sp := g.Dijkstra(src, chickpeas.Both, chickpeas.MatchAll(), w)
		for dst := chickpeas.NodeID(0); dst < n; dst++ {
			want, reachable := sp.Distance(dst)
			got, ok := g.WeightedShortestPath(src, dst, chickpeas.Both, chickpeas.MatchAll(), w)
			if ok != reachable {
				t.Fatalf("%d->%d: reachability disagrees (wsp %v, dijkstra %v)", src, dst, ok, reachable)
			}
			if ok && math.Abs(got-want) > 1e-9 {
				t.Fatalf("%d->%d: wsp %v, dijkstra %v", src, dst, got, want)
			}
		}
	}
}

func TestCanReach(t *testing.T) {
	g := weightedFixture(t)
	all := chickpeas.MatchAll()
	if !g.CanReach(0, 3, chickpeas.Outgoing, all, chickpeas.NoMaxDepth) {
		t.Fatal("0 should reach 3")
	}
	if g.CanReach(3, 0, chickpeas.Outgoing, all, chickpeas.NoMaxDepth) {
		t.Fatal("3 should not reach 0 outgoing")
	}
	if !g.CanReach(3, 0, chickpeas.Both, all, chickpeas.NoMaxDepth) {
		t.Fatal("3 should reach 0 undirected")
	}
	// Depth bounds: 3 is 1 hop via the shortcut, 0->2 needs 2 hops.
	if !g.CanReach(0, 3, chickpeas.Outgoing, all, 1) {
		t.Fatal("shortcut within 1 hop missed")
	}
	if g.CanReach(0, 2, chickpeas.Outgoing, all, 1) {
		t.Fatal("2 hops found within 1")
	}
	if !g.CanReach(0, 2, chickpeas.Outgoing, all, 2) {
		t.Fatal("2 hops missed within 2")
	}
	if !g.CanReach(1, 1, chickpeas.Outgoing, chickpeas.MatchNone(), 0) {
		t.Fatal("self-reachability must hold")
	}
}

func TestBFS(t *testing.T) {
	g := weightedFixture(t)
	all := chickpeas.MatchAll()
	nodes, rels := g.BFS(nodeset.Of(0), chickpeas.Outgoing, all, nil, nil, chickpeas.NoMaxDepth)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 1, 2, 3}) {
		t.Fatalf("nodes: %v", nodes.ToSlice())
	}
	if rels.Len() != 4 {
		t.Fatalf("rels: %v", rels.ToSlice())
	}
	// Depth 1: only direct neighbors (1 and the shortcut 3).
	nodes, _ = g.BFS(nodeset.Of(0), chickpeas.Outgoing, all, nil, nil, 1)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 1, 3}) {
		t.Fatalf("depth-1 nodes: %v", nodes.ToSlice())
	}
	// A node filter rejecting 1 blocks the chain but not the shortcut.
	notOne := func(n chickpeas.NodeID, _ *chickpeas.Snapshot) bool { return n != 1 }
	nodes, _ = g.BFS(nodeset.Of(0), chickpeas.Outgoing, all, notOne, nil, chickpeas.NoMaxDepth)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 3}) {
		t.Fatalf("filtered nodes: %v", nodes.ToSlice())
	}
	// A rel filter pruning the shortcut forces the chain.
	noShortcut := func(from, to chickpeas.NodeID, _ chickpeas.RelType, _ uint32, _ *chickpeas.Snapshot) bool {
		return !(from == 0 && to == 3)
	}
	nodes, rels = g.BFS(nodeset.Of(0), chickpeas.Outgoing, all, nil, noShortcut, chickpeas.NoMaxDepth)
	if !slices.Equal(nodes.ToSlice(), []uint32{0, 1, 2, 3}) || rels.Len() != 3 {
		t.Fatalf("rel-filtered: nodes %v rels %v", nodes.ToSlice(), rels.ToSlice())
	}
}

func TestBidirectionalBFS(t *testing.T) {
	g := weightedFixture(t)
	all := chickpeas.MatchAll()
	nodes, rels := g.BidirectionalBFS(nodeset.Of(0), nodeset.Of(3), chickpeas.Outgoing, all, nil, nil, chickpeas.NoMaxDepth)
	if nodes.IsEmpty() || rels.IsEmpty() {
		t.Fatal("connected sets found no meeting")
	}
	// Immediate overlap short-circuits with no rels.
	nodes, rels = g.BidirectionalBFS(nodeset.Of(0, 2), nodeset.Of(2), chickpeas.Outgoing, all, nil, nil, chickpeas.NoMaxDepth)
	if !slices.Equal(nodes.ToSlice(), []uint32{2}) || !rels.IsEmpty() {
		t.Fatalf("overlap: %v/%v", nodes.ToSlice(), rels.ToSlice())
	}
	// Disconnected sets (an isolated node) yield empty results.
	b := chickpeas.NewBuilder(4, 2)
	b.AddNodeWithID(0, "N")
	b.AddNodeWithID(1, "N")
	b.AddNodeWithID(2, "N")
	b.AddRel(0, 1, "R")
	iso := b.Finalize()
	nodes, rels = iso.BidirectionalBFS(nodeset.Of(0), nodeset.Of(2), chickpeas.Outgoing, chickpeas.MatchAll(), nil, nil, chickpeas.NoMaxDepth)
	if !nodes.IsEmpty() || !rels.IsEmpty() {
		t.Fatal("disconnected sets met")
	}
}

func TestBFSDistancesAndNeighborhood(t *testing.T) {
	g := weightedFixture(t)
	all := chickpeas.MatchAll()
	dist := g.BFSDistances(0, chickpeas.Outgoing, all, chickpeas.NoMaxDepth)
	want := map[chickpeas.NodeID]uint32{0: 0, 1: 1, 2: 2, 3: 1}
	if len(dist) != len(want) {
		t.Fatalf("distances: %v", dist)
	}
	for node, d := range want {
		if dist[node] != d {
			t.Fatalf("dist[%d]: got %d, want %d", node, dist[node], d)
		}
	}
	// Depth bound stops expansion past 1 hop.
	dist = g.BFSDistances(0, chickpeas.Outgoing, all, 1)
	if len(dist) != 3 {
		t.Fatalf("bounded distances: %v", dist)
	}
	// A start outside the id space is empty, not a panic.
	if d := g.BFSDistances(99, chickpeas.Outgoing, all, chickpeas.NoMaxDepth); len(d) != 0 {
		t.Fatal("out-of-space start returned distances")
	}

	// Neighborhood ranges against the distance map.
	if got := g.Neighborhood(0, chickpeas.Outgoing, all, 0, 2); !slices.Equal(got.ToSlice(), []uint32{0, 1, 2, 3}) {
		t.Fatalf("0..2: %v", got.ToSlice())
	}
	if got := g.Neighborhood(0, chickpeas.Outgoing, all, 1, 1); !slices.Equal(got.ToSlice(), []uint32{1, 3}) {
		t.Fatalf("1..1: %v", got.ToSlice())
	}
	if got := g.Neighborhood(0, chickpeas.Outgoing, all, 2, 2); !slices.Equal(got.ToSlice(), []uint32{2}) {
		t.Fatalf("2..2: %v", got.ToSlice())
	}
	if got := g.Neighborhood(0, chickpeas.Outgoing, all, 3, 2); !got.IsEmpty() {
		t.Fatal("inverted range not empty")
	}
	if got := g.Neighborhood(99, chickpeas.Outgoing, all, 0, 2); !got.IsEmpty() {
		t.Fatal("out-of-space seed not empty")
	}
}

// TestSearchScratchConcurrency drives the pooled gen-stamped scratch from
// many goroutines (race detector) and checks results stay correct.
func TestSearchScratchConcurrency(t *testing.T) {
	g := fixture(t, "big")
	all := chickpeas.MatchAll()
	baseline := g.BFSDistances(0, chickpeas.Both, all, 3)
	done := make(chan bool, 8)
	for range 8 {
		go func() {
			ok := true
			for range 20 {
				d := g.BFSDistances(0, chickpeas.Both, all, 3)
				ok = ok && len(d) == len(baseline)
				n := g.Neighborhood(0, chickpeas.Both, all, 1, 2)
				ok = ok && !n.IsEmpty()
			}
			done <- ok
		}()
	}
	for range 8 {
		if !<-done {
			t.Fatal("concurrent search returned inconsistent results")
		}
	}
}
