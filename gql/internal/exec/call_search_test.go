package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestCallSearchHits covers the index-backed search dispatch: full-text
// search and the two geo predicates each return the matching nodes and
// exclude the non-matching one.
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
