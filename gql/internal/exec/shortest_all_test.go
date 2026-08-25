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

// TestAllShortestComplete pins the complete-enumeration decision (task
// 244): a chain of 12 diamonds yields 2^12 = 4096 distinct minimum-hop
// paths, well past the removed 1024 safety valve, and every one must be
// enumerated -- distinct, hop-minimal, and rel-resolved. The fixture is
// also the memoized-predecessor path's regression: shared interior
// nodes are visited across thousands of paths and must not re-scan
// their adjacency per path.
func TestAllShortestComplete(t *testing.T) {
	const k = 12 // diamonds -> 2^k shortest paths
	bld := chickpeas.NewBuilder(64, 64)
	// v0 -> {a_i, b_i} -> v_i chained: node ids are 3i (junction),
	// 3i+1/3i+2 (the diamond pair) for i in 0..k.
	for range 3*k + 1 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := range k {
		j, a, b, next := graph.NodeID(3*i), graph.NodeID(3*i+1), graph.NodeID(3*i+2), graph.NodeID(3*(i+1))
		for _, e := range [][2]graph.NodeID{{j, a}, {j, b}, {a, next}, {b, next}} {
			if _, err := bld.AddRel(e[0], e[1], "R"); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx := &eval.Ctx{G: graph.New(bld.Finalize())}
	rm := ctx.G.CompileRelMatcher([]string{"R"})
	scr := newSPScratch()
	sp := &plan.SpStage{Dir: graph.Outgoing, Types: []string{"R"}}

	paths := allShortestPaths(ctx, 0, graph.NodeID(3*k), sp, rm, nil, scr)
	if len(paths) != 1<<k {
		t.Fatalf("complete enumeration = %d paths, want %d (the removed cap was 1024)", len(paths), 1<<k)
	}
	wantLen := 2*k + 1
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if len(p.nodes) != wantLen {
			t.Fatalf("path length %d, want %d (non-minimal path enumerated)", len(p.nodes), wantLen)
		}
		if len(p.rels) != wantLen-1 {
			t.Fatalf("path rels = %d, want %d", len(p.rels), wantLen-1)
		}
		key := ""
		for _, n := range p.nodes {
			key += string(rune(n)) + ","
		}
		if seen[key] {
			t.Fatal("duplicate path enumerated")
		}
		seen[key] = true
	}
}
