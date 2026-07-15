package chickpeas_test

import (
	"math"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func buildGraph(t *testing.T, n uint32, rels [][2]uint32) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(int(n), len(rels))
	for i := range n {
		b.AddNodeWithID(i, "V")
	}
	for _, r := range rels {
		b.AddRel(r[0], r[1], "e")
	}
	return b.Finalize()
}

func TestSSSP(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "V")
	}
	for _, r := range []struct {
		u, v chickpeas.NodeID
		w    float64
	}{{0, 1, 2}, {1, 2, 3}, {0, 2, 10}} {
		idx, _ := b.AddRel(r.u, r.v, "e")
		b.SetRelPropAt(idx, "weight", r.w)
	}
	g := b.Finalize()
	d := g.SSSP(0, true, "weight")
	if d[0] != 0 || d[1] != 2 || d[2] != 5 || !math.IsInf(d[3], 1) {
		t.Fatalf("sssp: %v", d)
	}
	// Unit weights.
	if d := g.SSSP(0, true, ""); d[2] != 1 {
		t.Fatalf("unit sssp: %v", d)
	}
}

func TestWCC(t *testing.T) {
	g := buildGraph(t, 5, [][2]uint32{{0, 1}, {1, 2}, {3, 4}})
	if got := g.WCC(); !slices.Equal(got, []uint32{0, 0, 0, 3, 3}) {
		t.Fatalf("wcc: %v", got)
	}
	// Type-filtered flooding.
	b := chickpeas.NewBuilder(4, 2)
	for i := range chickpeas.NodeID(4) {
		b.AddNodeWithID(i, "V")
	}
	b.AddRel(0, 1, "e")
	b.AddRel(2, 3, "f")
	g2 := b.Finalize()
	if got := g2.WCCVia(g2.Match("e"), chickpeas.Both); !slices.Equal(got, []uint32{0, 0, 2, 3}) {
		t.Fatalf("wcc via e: %v", got)
	}
	if got := g2.WCCVia(g2.Match("f"), chickpeas.Both); !slices.Equal(got, []uint32{0, 1, 2, 2}) {
		t.Fatalf("wcc via f: %v", got)
	}
}

func TestPageRank(t *testing.T) {
	// Uniform on a directed 3-cycle; ranks sum to 1.
	g := buildGraph(t, 3, [][2]uint32{{0, 1}, {1, 2}, {2, 0}})
	pr := g.PageRank(true, 0.85, 30)
	sum := 0.0
	for _, p := range pr {
		if math.Abs(p-1.0/3.0) > 1e-9 {
			t.Fatalf("cycle rank: %v", pr)
		}
		sum += p
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("ranks sum to %v", sum)
	}
	// Sink redistribution after one iteration (the Rust-pinned values).
	g2 := buildGraph(t, 2, [][2]uint32{{0, 1}})
	pr2 := g2.PageRank(true, 0.85, 1)
	if math.Abs(pr2[0]-0.2875) > 1e-9 || math.Abs(pr2[1]-0.7125) > 1e-9 {
		t.Fatalf("sink rank: %v", pr2)
	}
	if g2.PageRank(true, 0.85, 0)[0] != 0.5 {
		t.Fatal("zero iterations should keep the uniform seed")
	}
}

func TestCDLP(t *testing.T) {
	g := buildGraph(t, 3, [][2]uint32{{0, 1}, {1, 2}, {2, 0}})
	if got := g.CDLP(false, 2); !slices.Equal(got, []uint32{0, 0, 0}) {
		t.Fatalf("cdlp: %v", got)
	}
	if got := g.CDLPSeeded(false, 2, []uint32{10, 20, 30}); !slices.Equal(got, []uint32{10, 10, 10}) {
		t.Fatalf("seeded cdlp: %v", got)
	}
	// A short seed slice defaults missing slots to the node id.
	if got := g.CDLPSeeded(false, 0, []uint32{7}); !slices.Equal(got, []uint32{7, 1, 2}) {
		t.Fatalf("short seed: %v", got)
	}
}

func TestLCC(t *testing.T) {
	// Triangle with a pendant.
	g := buildGraph(t, 4, [][2]uint32{{0, 1}, {1, 2}, {2, 0}, {0, 3}})
	c := g.LCC(false)
	if math.Abs(c[0]-1.0/3.0) > 1e-9 || math.Abs(c[1]-1) > 1e-9 || math.Abs(c[2]-1) > 1e-9 || c[3] != 0 {
		t.Fatalf("lcc: %v", c)
	}
	// A high-degree neighbor exercises the gallop branch: LCC(0) = 0.5.
	g2 := buildGraph(t, 5, [][2]uint32{{0, 1}, {0, 2}, {1, 2}, {1, 3}, {1, 4}})
	if got := g2.LCC(true)[0]; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("gallop lcc: %v", got)
	}
}

// TestAnalyticsSparseIDs ports the sparse-id sweep: ids {0, 1, 100} give a
// 101-slot id space; every kernel must size by it, not NodeCount.
func TestAnalyticsSparseIDs(t *testing.T) {
	b := chickpeas.NewBuilder(4, 2)
	for _, id := range []chickpeas.NodeID{0, 1, 100} {
		b.AddNodeWithID(id, "V")
	}
	b.AddRel(0, 100, "e")
	b.AddRel(1, 100, "e")
	g := b.Finalize()

	comp := g.WCC()
	if len(comp) != 101 || comp[0] != comp[100] || comp[1] != comp[100] {
		t.Fatalf("sparse wcc: len %d", len(comp))
	}
	if len(g.PageRank(true, 0.85, 5)) != 101 {
		t.Fatal("sparse pagerank length")
	}
	if len(g.CDLP(false, 2)) != 101 {
		t.Fatal("sparse cdlp length")
	}
	if len(g.CDLPSeeded(false, 2, []uint32{7, 8})) != 101 {
		t.Fatal("sparse seeded cdlp length")
	}
	if len(g.LCC(false)) != 101 {
		t.Fatal("sparse lcc length")
	}
	if len(g.SSSP(0, false, "")) != 101 {
		t.Fatal("sparse sssp length")
	}
}

// TestSSSPUnitWeightsMatchDijkstra pins the unit-weight BFS dispatch (the
// third spelling of the constant-weight-Dijkstra bug, found via the
// rustychickpeas re-grep prescription: it hid in the procedure surface,
// not the path stage): SSSP with no weight key must equal a unit-weight
// Dijkstra on every node, +Inf included.
func TestSSSPUnitWeightsMatchDijkstra(t *testing.T) {
	b := chickpeas.NewBuilder(64, 256)
	for range 40 {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	seed := uint64(0xABCD)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for range 120 {
		b.AddRel(chickpeas.NodeID(next()%40), chickpeas.NodeID(next()%40), "R")
	}
	g := b.Finalize()
	for _, directed := range []bool{true, false} {
		got := g.SSSP(3, directed, "")
		dir := chickpeas.Both
		if directed {
			dir = chickpeas.Outgoing
		}
		ref := g.Dijkstra(3, dir, chickpeas.MatchAll(), func(chickpeas.NodeID, chickpeas.RelRef) float64 { return 1 })
		for v := range got {
			want, ok := ref.Distance(chickpeas.NodeID(v))
			if !ok {
				want = math.Inf(1)
			}
			if got[v] != want {
				t.Fatalf("directed=%v node %d: BFS sssp %v != unit dijkstra %v", directed, v, got[v], want)
			}
		}
	}
	// A weight key with no column is unit weights too.
	got := g.SSSP(3, true, "nosuchkey")
	ref := g.SSSP(3, true, "")
	for v := range got {
		if got[v] != ref[v] {
			t.Fatalf("missing-column weights diverge at %d: %v vs %v", v, got[v], ref[v])
		}
	}
}
