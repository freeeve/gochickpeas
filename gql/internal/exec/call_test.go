package exec

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestPerNodeValues covers the CALL analytics dispatch and its value-kind
// contract (which mirrors the Rust engine): WCC and BFS return Int, PageRank
// returns Float, an unreachable BFS node is MaxInt64, and the index-backed
// search procedures are not per-node (ok=false). Fixture: a connected chain
// 0-1-2 with an isolated node 3.
func TestPerNodeValues(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("N")
	n1, _ := b.AddNode("N")
	n2, _ := b.AddNode("N")
	_, _ = b.AddNode("N") // node 3: isolated
	_, err := b.AddRel(n0, n1, "R")
	must(err)
	_, err = b.AddRel(n1, n2, "R")
	must(err)
	// Weights so the weighted-SSSP branch has a column to read.
	must(b.SetRelProp(n0, n1, "R", "weight", 2.0))
	must(b.SetRelProp(n1, n2, "R", "weight", 3.0))
	g := b.Finalize("pnv")

	// WccAll: component ids as Int; the connected chain shares a component,
	// the isolated node has its own.
	wcc, ok := perNodeValues(&plan.CallProc{Kind: plan.ProcWccAll}, g)
	if !ok {
		t.Fatal("wcc must be a per-node procedure")
	}
	for i, v := range wcc {
		if v.Kind() != value.KindInt {
			t.Fatalf("wcc[%d] kind = %v, want Int", i, v.Kind())
		}
	}
	c0, _ := wcc[0].AsInt()
	c1, _ := wcc[1].AsInt()
	c2, _ := wcc[2].AsInt()
	c3, _ := wcc[3].AsInt()
	if c0 != c1 || c1 != c2 {
		t.Fatalf("chain nodes must share a component: %d %d %d", c0, c1, c2)
	}
	if c3 == c0 {
		t.Fatal("the isolated node must not share the chain's component")
	}

	// Bfs from node 0 (undirected): Int distances, MaxInt64 for the
	// unreachable isolated node.
	bfs, ok := perNodeValues(&plan.CallProc{Kind: plan.ProcBfs, Source: graph.NodeID(n0)}, g)
	if !ok {
		t.Fatal("bfs must be a per-node procedure")
	}
	for i, want := range []int64{0, 1, 2, math.MaxInt64} {
		if bfs[i].Kind() != value.KindInt {
			t.Fatalf("bfs[%d] kind = %v, want Int", i, bfs[i].Kind())
		}
		if got, _ := bfs[i].AsInt(); got != want {
			t.Fatalf("bfs[%d] = %d, want %d", i, got, want)
		}
	}

	// PageRank: a Float per node.
	pr, ok := perNodeValues(&plan.CallProc{Kind: plan.ProcPageRank, Damping: 0.85, Iters: 20}, g)
	if !ok {
		t.Fatal("pagerank must be a per-node procedure")
	}
	for i, v := range pr {
		if v.Kind() != value.KindFloat {
			t.Fatalf("pagerank[%d] kind = %v, want Float", i, v.Kind())
		}
	}

	// The remaining analytics procedures, each pinned to its value kind
	// (the Rust-parity contract): typed-WCC and CDLP are Int, LCC and SSSP
	// are Float.
	for _, tc := range []struct {
		name string
		proc *plan.CallProc
		kind value.Kind
	}{
		{"wcc", &plan.CallProc{Kind: plan.ProcWcc, RelType: "R", Direction: graph.Outgoing}, value.KindInt},
		{"cdlp", &plan.CallProc{Kind: plan.ProcCdlp, Iters: 5}, value.KindInt},
		{"lcc", &plan.CallProc{Kind: plan.ProcLcc}, value.KindFloat},
		{"sssp", &plan.CallProc{Kind: plan.ProcSssp, Source: graph.NodeID(n0)}, value.KindFloat},
		{"sssp-weighted", &plan.CallProc{Kind: plan.ProcSssp, Source: graph.NodeID(n0), Weighted: true}, value.KindFloat},
	} {
		vals, ok := perNodeValues(tc.proc, g)
		if !ok {
			t.Fatalf("%s must be a per-node procedure", tc.name)
		}
		for i, v := range vals {
			if v.Kind() != tc.kind {
				t.Fatalf("%s[%d] kind = %v, want %v", tc.name, i, v.Kind(), tc.kind)
			}
		}
	}

	// An index-backed search procedure is not per-node.
	if _, ok := perNodeValues(&plan.CallProc{Kind: plan.ProcFtsSearch}, g); ok {
		t.Fatal("fts.search must not be a per-node procedure")
	}
}

func TestIntFloatValues(t *testing.T) {
	iv := intValues([]uint32{0, 5, 9})
	if len(iv) != 3 {
		t.Fatalf("intValues len = %d, want 3", len(iv))
	}
	for i, want := range []int64{0, 5, 9} {
		if iv[i].Kind() != value.KindInt {
			t.Fatalf("intValues[%d] kind = %v, want Int", i, iv[i].Kind())
		}
		if got, _ := iv[i].AsInt(); got != want {
			t.Fatalf("intValues[%d] = %d, want %d", i, got, want)
		}
	}
	if len(intValues(nil)) != 0 {
		t.Fatal("intValues(nil) must be empty")
	}

	fv := floatValues([]float64{1.5, -2.0})
	if len(fv) != 2 {
		t.Fatalf("floatValues len = %d, want 2", len(fv))
	}
	for i, want := range []float64{1.5, -2.0} {
		if fv[i].Kind() != value.KindFloat {
			t.Fatalf("floatValues[%d] kind = %v, want Float", i, fv[i].Kind())
		}
		if got, _ := fv[i].AsFloat(); got != want {
			t.Fatalf("floatValues[%d] = %v, want %v", i, got, want)
		}
	}
}

// TestCdlpInit covers CDLP label seeding: an int seed property seeds each
// node that carries it, and every other case -- no seed property, a missing
// value, a non-existent column, and a non-i64 column -- falls back to the
// dense node id.
func TestCdlpInit(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("N")
	must(b.SetProp(n0, "seed", int64(100)))
	n1, _ := b.AddNode("N")
	must(b.SetProp(n1, "seed", int64(200)))
	n2, _ := b.AddNode("N")
	must(b.SetProp(n2, "name", "x")) // string column, no seed
	_, _ = b.AddNode("N")            // node 3: no properties
	g := b.Finalize("cdlp")

	denseIDs := func(init []uint32, ctx string) {
		t.Helper()
		for i := range init {
			if init[i] != uint32(i) {
				t.Fatalf("%s: init[%d] = %d, want dense id %d", ctx, i, init[i], i)
			}
		}
	}

	// No seed property: every label is the dense id.
	denseIDs(cdlpInit(g, ""), "empty seedProp")
	// A non-existent column: dense ids.
	denseIDs(cdlpInit(g, "nonexistent"), "unknown column")
	// A non-i64 (string) column: dense ids.
	denseIDs(cdlpInit(g, "name"), "string column")

	// The int seed column seeds where present, dense id where absent.
	got := cdlpInit(g, "seed")
	if got[0] != 100 || got[1] != 200 {
		t.Fatalf("seeded labels = %d,%d, want 100,200", got[0], got[1])
	}
	if got[2] != 2 || got[3] != 3 {
		t.Fatalf("unseeded labels = %d,%d, want dense ids 2,3", got[2], got[3])
	}
}

func TestCallPropagateRows(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("N")
	n1, _ := b.AddNode("N")
	n2, _ := b.AddNode("N")
	_, _ = b.AddNode("N") // node 3: isolated
	i0, err := b.AddRel(n0, n1, "R")
	must(err)
	must(b.SetRelPropAt(i0, "w", 5.0))
	i1, err := b.AddRel(n1, n2, "R")
	must(err)
	must(b.SetRelPropAt(i1, "w", 7.0))
	g := b.Finalize("prop")

	byNode := func(res []chickpeas.PropagateResult) map[chickpeas.NodeID]chickpeas.PropagateResult {
		m := map[chickpeas.NodeID]chickpeas.PropagateResult{}
		for _, r := range res {
			m[r.Node] = r
		}
		return m
	}

	full := &plan.CallProc{
		Kind:      plan.ProcPropagate,
		Seeds:     []graph.NodeID{graph.NodeID(n0)},
		SeedVals:  []float64{10.0},
		RelTypes:  []string{"R"},
		Direction: chickpeas.Outgoing,
		MaxDepth:  5,
		ValueProp: "w",
		MinValue:  math.Inf(-1),
	}
	m := byNode(callPropagateRows(full, g))
	// The seed carries its own value at depth 1; each claimed node takes the
	// claiming rel's ValueProp; the isolated node is never reached.
	if len(m) != 3 {
		t.Fatalf("reached %d nodes, want 3 (isolated node excluded)", len(m))
	}
	if r := m[graph.NodeID(n0)]; r.Value != 10 || r.Depth != 1 {
		t.Fatalf("seed = %+v, want value 10 depth 1", r)
	}
	if r := m[graph.NodeID(n1)]; r.Value != 5 || r.Depth != 2 {
		t.Fatalf("n1 = %+v, want value 5 depth 2", r)
	}
	if r := m[graph.NodeID(n2)]; r.Value != 7 || r.Depth != 3 {
		t.Fatalf("n2 = %+v, want value 7 depth 3", r)
	}

	// MaxDepth 1 keeps seeds only (they sit at depth 1 and expand only while
	// depth < MaxDepth).
	seedsOnly := *full
	seedsOnly.MaxDepth = 1
	if m := byNode(callPropagateRows(&seedsOnly, g)); len(m) != 1 || m[graph.NodeID(n0)].Value != 10 {
		t.Fatalf("MaxDepth 1 reached %d nodes, want the seed only", len(m))
	}
}

func TestCallSearchHits(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	near, _ := b.AddNode("Place")
	must(b.SetProp(near, "body", "hello world"))
	must(b.SetProp(near, "lat", 40.0))
	must(b.SetProp(near, "lon", -74.0))
	far, _ := b.AddNode("Place")
	must(b.SetProp(far, "body", "goodbye there"))
	must(b.SetProp(far, "lat", 0.0))
	must(b.SetProp(far, "lon", 0.0))
	g := b.Finalize("search")

	only := func(name string, hits interface {
		Len() int
		Contains(uint32) bool
	}) {
		t.Helper()
		if hits == nil || hits.Len() != 1 || !hits.Contains(uint32(near)) || hits.Contains(uint32(far)) {
			t.Fatalf("%s hits = %v, want {near} only", name, hits)
		}
	}

	only("fts", callSearchHits(&plan.CallProc{
		Kind: plan.ProcFtsSearch, Label: "Place", Field: "body", Query: "world"}, g))
	only("geo-radius", callSearchHits(&plan.CallProc{
		Kind: plan.ProcGeoWithinRadius, Label: "Place", LatField: "lat", LonField: "lon",
		Lat: 40.0, Lon: -74.0, Km: 50}, g))
	only("geo-bbox", callSearchHits(&plan.CallProc{
		Kind: plan.ProcGeoWithinBBox, Label: "Place", LatField: "lat", LonField: "lon",
		MinLat: 39, MinLon: -75, MaxLat: 41, MaxLon: -73}, g))
}

// TestCallResultsDispatch covers the three result shapes callResults routes
// to: a search procedure yields a hit-set iterator, a per-node analytics
// procedure a value vector, and a propagate procedure its rows -- each with
// the other two return channels nil.
func TestCallResultsDispatch(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("Doc")
	must(b.SetProp(n0, "body", "alpha world"))
	n1, _ := b.AddNode("Doc")
	_, err := b.AddRel(n0, n1, "R")
	must(err)
	g := b.Finalize("dispatch")

	// Search -> hit-set iterator only.
	vals, iter, prop := callResults(&plan.CallProc{
		Kind: plan.ProcFtsSearch, Label: "Doc", Field: "body", Query: "world"}, g)
	if vals != nil || iter == nil || prop != nil {
		t.Fatalf("search dispatch: vals=%v iter=%v prop=%v", vals != nil, iter != nil, prop != nil)
	}
	n := 0
	for range iter {
		n++
	}
	if n != 1 {
		t.Fatalf("search iter yielded %d nodes, want 1", n)
	}

	// Per-node analytics -> value vector only.
	vals, iter, prop = callResults(&plan.CallProc{Kind: plan.ProcWccAll}, g)
	if vals == nil || iter != nil || prop != nil {
		t.Fatalf("per-node dispatch: vals=%v iter=%v prop=%v", vals != nil, iter != nil, prop != nil)
	}

	// Propagate -> propagate rows only.
	vals, iter, prop = callResults(&plan.CallProc{
		Kind: plan.ProcPropagate, Seeds: []graph.NodeID{graph.NodeID(n0)}, SeedVals: []float64{1.0},
		RelTypes: []string{"R"}, Direction: chickpeas.Outgoing, MaxDepth: 3}, g)
	if vals != nil || iter != nil || prop == nil {
		t.Fatalf("propagate dispatch: vals=%v iter=%v prop=%v", vals != nil, iter != nil, prop != nil)
	}

	// A search over an unknown label still routes to the search shape, with
	// an empty (but valid) iterator.
	vals, iter, prop = callResults(&plan.CallProc{
		Kind: plan.ProcFtsSearch, Label: "NoSuchLabel", Field: "body", Query: "world"}, g)
	if vals != nil || iter == nil || prop != nil {
		t.Fatalf("empty search dispatch: vals=%v iter=%v prop=%v", vals != nil, iter != nil, prop != nil)
	}
	n = 0
	for range iter {
		n++
	}
	if n != 0 {
		t.Fatalf("unknown-label search yielded %d nodes, want 0", n)
	}
}
