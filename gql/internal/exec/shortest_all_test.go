package exec

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestAllShortestPaths covers the all-minimum-hop enumeration (and the
// enumeratePaths backward DFS it drives) over a diamond with two equal-length
// paths a->d, the trivial a==a path, and an unreachable target.
func TestAllShortestPaths(t *testing.T) {
	// 0->1->3 and 0->2->3 (a diamond); node 4 is isolated.
	bld := chickpeas.NewBuilder(8, 8)
	for range 5 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}} {
		if _, err := bld.AddRel(graph.NodeID(e[0]), graph.NodeID(e[1]), "R"); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &eval.Ctx{G: graph.New(bld.Finalize())}
	rm := ctx.G.CompileRelMatcher([]string{"R"})
	scr := newSPScratch()
	sp := &plan.SpStage{Dir: graph.Outgoing, Types: []string{"R"}}

	// The diamond yields two distinct minimum-hop paths of three nodes each.
	paths := allShortestPaths(ctx, 0, 3, sp, rm, nil, scr)
	if len(paths) != 2 {
		t.Fatalf("diamond paths = %d, want 2", len(paths))
	}
	has := func(want []graph.NodeID) bool {
		for _, p := range paths {
			if slices.Equal(p.nodes, want) {
				return true
			}
		}
		return false
	}
	if !has([]graph.NodeID{0, 1, 3}) || !has([]graph.NodeID{0, 2, 3}) {
		t.Fatalf("diamond paths = %v, want {0,1,3} and {0,2,3}", paths)
	}
	// Each path resolves one rel per hop.
	for _, p := range paths {
		if len(p.rels) != 2 {
			t.Fatalf("path %v rels = %v, want 2", p.nodes, p.rels)
		}
	}

	// a == b is the single trivial path [a].
	self := allShortestPaths(ctx, 0, 0, sp, rm, nil, scr)
	if len(self) != 1 || !slices.Equal(self[0].nodes, []graph.NodeID{0}) {
		t.Fatalf("self path = %v, want [[0]]", self)
	}

	// An unreachable target yields no paths.
	if p := allShortestPaths(ctx, 0, 4, sp, rm, nil, scr); p != nil {
		t.Fatalf("unreachable target paths = %v, want nil", p)
	}
}
