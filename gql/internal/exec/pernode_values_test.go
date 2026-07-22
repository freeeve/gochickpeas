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
