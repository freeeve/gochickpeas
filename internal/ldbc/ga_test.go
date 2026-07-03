package ldbc

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// gaBuild wires n nodes with the given e rels (optionally weighted).
func gaBuild(t *testing.T, n int, rels [][2]uint32, weights []float64) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, len(rels))
	for range n {
		if _, err := b.AddNode("V"); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range rels {
		idx, err := b.AddRel(r[0], r[1], "e")
		if err != nil {
			t.Fatal(err)
		}
		if weights != nil {
			if err := b.SetRelPropAt(idx, "weight", weights[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize()
}

func TestGALoaderMapsVerticesAndParams(t *testing.T) {
	const v = "10\n20\n30\n"
	const e = "10 20 2.5\n20 30 4.0\n10 30\n"
	const props = `
graph.x.directed = false
graph.x.bfs.source-vertex = 20
graph.x.pr.damping-factor = 0.85
graph.x.pr.num-iterations = 7
graph.x.cdlp.max-iterations = 9
graph.x.sssp.source-vertex = 10
`
	ds, err := loadGAStr(v, e, props)
	if err != nil {
		t.Fatal(err)
	}
	if got := ds.VertexOfNode; len(got) != 3 || got[0] != 10 || got[2] != 30 {
		t.Fatalf("vertexOfNode = %v", got)
	}
	if n, ok := ds.Node(30); !ok || n != 2 {
		t.Fatalf("Node(30) = %d, %v", n, ok)
	}
	if _, ok := ds.Node(99); ok {
		t.Fatal("Node(99) should be absent")
	}
	p := ds.Params
	if p.Directed || p.BFSSource == nil || *p.BFSSource != 20 || p.SSSPSource == nil ||
		*p.SSSPSource != 10 || p.PRIterations != 7 || p.CDLPIterations != 9 {
		t.Fatalf("params = %+v", p)
	}
	dflt, err := loadGAStr(v, e, "")
	if err != nil {
		t.Fatal(err)
	}
	if !dflt.Params.Directed || dflt.Params.PRIterations != 10 || dflt.Params.BFSSource != nil {
		t.Fatalf("default params = %+v", dflt.Params)
	}
}

func TestGABFSDepthsAndUnreachable(t *testing.T) {
	g := gaBuild(t, 4, [][2]uint32{{0, 1}, {1, 2}}, nil)
	got := GABFS(g, 0, true)
	want := []int64{0, 1, 2, math.MaxInt64}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bfs[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestGASSSPWeightedAndUnreachable(t *testing.T) {
	g := gaBuild(t, 4, [][2]uint32{{0, 1}, {1, 2}, {0, 2}}, []float64{2, 3, 10})
	d := GASSSP(g, 0, true)
	if d[0] != 0 || d[1] != 2 || d[2] != 5 || !math.IsInf(d[3], 1) {
		t.Fatalf("sssp = %v", d)
	}
}

func TestGAWCCTwoComponents(t *testing.T) {
	g := gaBuild(t, 5, [][2]uint32{{0, 1}, {1, 2}, {3, 4}}, nil)
	got := GAWCC(g)
	want := []uint32{0, 0, 0, 3, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wcc = %v, want %v", got, want)
		}
	}
}

func TestGAPageRankCycleAndSink(t *testing.T) {
	cyc := gaBuild(t, 3, [][2]uint32{{0, 1}, {1, 2}, {2, 0}}, nil)
	pr := GAPageRank(cyc, true, 0.85, 30)
	for _, p := range pr {
		if math.Abs(p-1.0/3.0) > 1e-9 {
			t.Fatalf("cycle pr = %v", pr)
		}
	}
	sink := gaBuild(t, 2, [][2]uint32{{0, 1}}, nil)
	pr = GAPageRank(sink, true, 0.85, 1)
	if math.Abs(pr[0]-0.2875) > 1e-9 || math.Abs(pr[1]-0.7125) > 1e-9 {
		t.Fatalf("sink pr = %v", pr)
	}
}

func TestGACDLPSeededTriangle(t *testing.T) {
	g := gaBuild(t, 3, [][2]uint32{{0, 1}, {1, 2}, {2, 0}}, nil)
	got := GACDLPSeeded(g, false, 2, []uint32{10, 20, 30})
	for _, l := range got {
		if l != 10 {
			t.Fatalf("cdlp = %v", got)
		}
	}
}

func TestGALCCTriangleWithPendant(t *testing.T) {
	g := gaBuild(t, 4, [][2]uint32{{0, 1}, {1, 2}, {2, 0}, {0, 3}}, nil)
	c := GALCC(g, false)
	if math.Abs(c[0]-1.0/3.0) > 1e-9 || math.Abs(c[1]-1.0) > 1e-9 || c[3] != 0 {
		t.Fatalf("lcc = %v", c)
	}
	// gallop branch: node 1's out-degree exceeds |N(0)|.
	g2 := gaBuild(t, 5, [][2]uint32{{0, 1}, {0, 2}, {1, 2}, {1, 3}, {1, 4}}, nil)
	c2 := GALCC(g2, true)
	if math.Abs(c2[0]-0.5) > 1e-9 {
		t.Fatalf("lcc gallop = %v", c2)
	}
}

func TestGAValidators(t *testing.T) {
	ds, err := loadGAStr("1\n2\n3\n", "", "")
	if err != nil {
		t.Fatal(err)
	}
	exact := ParseGAReference("1 0\n2 1\n3 2\n")
	if err := GACheckExactI64(ds, []int64{0, 1, 2}, exact); err != nil {
		t.Fatal(err)
	}
	if GACheckExactI64(ds, []int64{0, 9, 2}, exact) == nil {
		t.Fatal("exact should fail on diff")
	}
	eps := ParseGAReference("1 0.5\n2 0.25\n3 inf\n")
	if err := GACheckEpsilon(ds, []float64{0.5 + 1e-9, 0.25, math.Inf(1)}, eps, 1e-6); err != nil {
		t.Fatal(err)
	}
	if GACheckEpsilon(ds, []float64{0.5, 0.25, 1.0}, eps, 1e-6) == nil {
		t.Fatal("epsilon should fail on finite vs inf")
	}
	if GACheckEpsilon(ds, []float64{0.6, 0.25, math.Inf(1)}, eps, 1e-6) == nil {
		t.Fatal("epsilon should fail out of tolerance")
	}
	same := ParseGAReference("1 100\n2 100\n3 200\n")
	if err := GACheckRelabel(ds, []uint32{5, 5, 7}, same); err != nil {
		t.Fatal(err)
	}
	reshaped := ParseGAReference("1 100\n2 200\n3 200\n")
	if GACheckRelabel(ds, []uint32{5, 5, 7}, reshaped) == nil {
		t.Fatal("relabel should reject a reshaped partition")
	}
}
