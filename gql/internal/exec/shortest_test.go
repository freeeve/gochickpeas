// Weighted shortest-path kernel tests, building the SpStage directly
// (the COST clause's e2e coverage lives in the gql package) -- the
// cheapest route must win over the fewest-hop route, per-edge exclusions
// apply, and each CostSpec kind compiles.
package exec

import (
	"math/rand"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// weightedGraph: 0 -[w=10]-> 1 (direct), 0 -[w=1]-> 2 -[w=1]-> 1 (cheap
// detour).
func weightedGraph(t *testing.T) *eval.Ctx {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	for range 3 {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	edges := []struct {
		u, v chickpeas.NodeID
		w    float64
	}{{0, 1, 10}, {0, 2, 1}, {2, 1, 1}}
	for _, e := range edges {
		if _, err := b.AddRel(e.u, e.v, "R"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelProp(e.u, e.v, "R", "w", e.w); err != nil {
			t.Fatal(err)
		}
	}
	return &eval.Ctx{G: graph.New(b.Finalize())}
}

// runWeighted binds path p between nodes 0 and 1 under the given weight.
func runWeighted(t *testing.T, ctx *eval.Ctx, weight *ast.CostSpec, weightVar string) value.Value {
	t.Helper()
	sp := &plan.SpStage{
		PathSlot: 2, From: 0, To: 1,
		Dir: graph.Outgoing, Types: []string{"R"},
		Weight: weight, WeightVar: weightVar,
	}
	rows := runSPStage(ctx, sp, [][]value.Value{{value.Node(0), value.Node(1), value.Null()}})
	if len(rows) != 1 {
		t.Fatalf("weighted sp rows = %d", len(rows))
	}
	return rows[0][2]
}

func TestWeightedShortestPathPrefersCheapRoute(t *testing.T) {
	ctx := weightedGraph(t)
	p := runWeighted(t, ctx, &ast.CostSpec{Kind: ast.CostProperty, Prop: "w"}, "")
	nodes, rels, ok := p.AsPath()
	if !ok || len(nodes) != 3 || nodes[0] != 0 || nodes[1] != 2 || nodes[2] != 1 {
		t.Fatalf("cheap route nodes = %v (ok=%v)", nodes, ok)
	}
	if len(rels) != 2 {
		t.Fatalf("cheap route rels = %v", rels)
	}
}

func TestWeightedConstantMinimizesHops(t *testing.T) {
	ctx := weightedGraph(t)
	// A constant weight makes every edge equal, so the direct edge wins.
	p := runWeighted(t, ctx, &ast.CostSpec{Kind: ast.CostConstant, Const: 1.0}, "")
	nodes, _, _ := p.AsPath()
	if len(nodes) != 2 || nodes[1] != 1 {
		t.Fatalf("constant-weight nodes = %v", nodes)
	}
	// An invalid constant degrades to unit weights (Missing), not no-path.
	p = runWeighted(t, ctx, &ast.CostSpec{Kind: ast.CostConstant, Const: -5}, "")
	if nodes, _, ok := p.AsPath(); !ok || len(nodes) != 2 {
		t.Fatalf("invalid constant should behave as unit weights, got %v", nodes)
	}
}

func TestWeightedExprFormula(t *testing.T) {
	ctx := weightedGraph(t)
	// r.w * 2 preserves the ordering: the detour still wins.
	formula := &ast.Binary{
		Op:  ast.OpMul,
		LHS: &ast.Prop{Var: "r", Key: "w"},
		RHS: &ast.Lit{Value: ast.IntLit(2)},
	}
	p := runWeighted(t, ctx, &ast.CostSpec{Kind: ast.CostExpr, Expr: formula}, "r")
	nodes, _, _ := p.AsPath()
	if len(nodes) != 3 || nodes[1] != 2 {
		t.Fatalf("formula route = %v", nodes)
	}
	// A formula yielding a negative weight excludes that edge: negating
	// the weight excludes every edge, so no path binds and the required
	// stage drops the row.
	neg := &ast.Unary{Op: ast.Neg, Expr: &ast.Prop{Var: "r", Key: "w"}}
	sp := &plan.SpStage{
		PathSlot: 2, From: 0, To: 1,
		Dir: graph.Outgoing, Types: []string{"R"},
		Weight: &ast.CostSpec{Kind: ast.CostExpr, Expr: neg}, WeightVar: "r",
	}
	rows := runSPStage(ctx, sp, [][]value.Value{{value.Node(0), value.Node(1), value.Null()}})
	if len(rows) != 0 {
		t.Fatalf("all-excluded edges should drop the row, got %d rows", len(rows))
	}
}

// refShortestLen is the independent oracle: a plain map-based BFS over the
// public neighbor seam, returning the minimum hop count a..b under the cap
// (-1 when unreachable within it). Deliberately naive -- it shares no code
// with the bidirectional search it checks.
func refShortestLen(ctx *eval.Ctx, a, b graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hopCap uint64) int {
	if a == b {
		return 0
	}
	visited := map[graph.NodeID]bool{a: true}
	frontier := []graph.NodeID{a}
	for depth := uint64(1); len(frontier) > 0 && depth <= hopCap; depth++ {
		var next []graph.NodeID
		for _, u := range frontier {
			for v := range ctx.G.NeighborsMatched(u, dir, rm) {
				if visited[v] {
					continue
				}
				if v == b {
					return int(depth)
				}
				visited[v] = true
				next = append(next, v)
			}
		}
		frontier = next
	}
	return -1
}

// TestShortestPathDifferential fuzzes the bidirectional search against the
// reference BFS across graph shapes, directions, and hop caps: found/not
// agreement, exact minimum length, AND that the returned node list is a
// real walk -- each consecutive pair joined by an accepted relationship in
// the pattern direction (a stitching bug yields a plausible node list that
// is not a path; the length assertion alone would pass it).
func TestShortestPathDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(97))
	shapes := []func(b *chickpeas.Builder, n int){
		// sparse random
		func(b *chickpeas.Builder, n int) {
			for range n * 3 {
				_, _ = b.AddRel(chickpeas.NodeID(rng.Intn(n)), chickpeas.NodeID(rng.Intn(n)), "R")
			}
		},
		// hub-heavy: node 0 fans out wide, plus a sparse mesh
		func(b *chickpeas.Builder, n int) {
			for i := 1; i < n; i += 2 {
				_, _ = b.AddRel(0, chickpeas.NodeID(i), "R")
			}
			for range n {
				_, _ = b.AddRel(chickpeas.NodeID(rng.Intn(n)), chickpeas.NodeID(rng.Intn(n)), "R")
			}
		},
		// long chain with random shortcuts
		func(b *chickpeas.Builder, n int) {
			for i := 0; i+1 < n; i++ {
				_, _ = b.AddRel(chickpeas.NodeID(i), chickpeas.NodeID(i+1), "R")
			}
			for range n / 4 {
				_, _ = b.AddRel(chickpeas.NodeID(rng.Intn(n)), chickpeas.NodeID(rng.Intn(n)), "R")
			}
		},
	}
	caps := []uint64{1, 2, 3, 1<<63 - 1}
	for si, shape := range shapes {
		const n = 200
		b := chickpeas.NewBuilder(n, n*4)
		for range n {
			if _, err := b.AddNode("N"); err != nil {
				t.Fatal(err)
			}
		}
		shape(b, n)
		ctx := &eval.Ctx{G: graph.New(b.Finalize())}
		rm := ctx.G.CompileRelMatcher([]string{"R"})
		scr := newSPScratch()
		for _, dir := range []graph.Direction{graph.Outgoing, graph.Incoming, graph.Both} {
			for trial := range 700 {
				a := graph.NodeID(rng.Intn(n))
				bb := graph.NodeID(rng.Intn(n))
				hopCap := caps[trial%len(caps)]
				sp := &plan.SpStage{Dir: dir, Types: []string{"R"}}
				if hopCap != 1<<63-1 {
					c := hopCap
					sp.Max = &c
				}
				path, found := shortestPath(ctx, a, bb, sp, rm, nil, scr)
				want := refShortestLen(ctx, a, bb, dir, rm, hopCap)
				if !found {
					if want != -1 {
						t.Fatalf("shape %d dir %v cap %d (%d,%d): no path found, reference length %d", si, dir, hopCap, a, bb, want)
					}
					continue
				}
				got := len(path.nodes) - 1
				if got != want {
					t.Fatalf("shape %d dir %v cap %d (%d,%d): length %d, reference %d", si, dir, hopCap, a, bb, got, want)
				}
				if path.nodes[0] != a || path.nodes[got] != bb {
					t.Fatalf("shape %d (%d,%d): endpoints %v", si, a, bb, path.nodes)
				}
				for i := 0; i < got; i++ {
					if ctx.G.CountNeighborsMatched(path.nodes[i], path.nodes[i+1], dir, rm) == 0 {
						t.Fatalf("shape %d dir %v (%d,%d): nodes %v is not a walk -- no accepted rel %d->%d",
							si, dir, a, bb, path.nodes, path.nodes[i], path.nodes[i+1])
					}
				}
				if len(path.rels) != got {
					t.Fatalf("shape %d (%d,%d): %d rels for %d hops", si, a, bb, len(path.rels), got)
				}
			}
		}
	}
}

func TestWeightedHopCap(t *testing.T) {
	ctx := weightedGraph(t)
	one := uint64(1)
	sp := &plan.SpStage{
		PathSlot: 2, From: 0, To: 1,
		Dir: graph.Outgoing, Types: []string{"R"}, Max: &one,
		Weight: &ast.CostSpec{Kind: ast.CostProperty, Prop: "w"},
	}
	rows := runSPStage(ctx, sp, [][]value.Value{{value.Node(0), value.Node(1), value.Null()}})
	if len(rows) != 1 {
		t.Fatalf("capped rows = %d", len(rows))
	}
	// The 2-hop detour exceeds the cap, so the expensive direct edge wins.
	nodes, _, _ := rows[0][2].AsPath()
	if len(nodes) != 2 {
		t.Fatalf("capped route = %v", nodes)
	}
}

// TestMissingWeightColumnTakesBFS pins the third degraded shape of the
// constant-cost bug (128): COST r.<key> where the key has NO column reads
// 1.0 on every edge -- unit weights through the property door -- so it
// must take the BFS forms. The oracle is the weighted-search counter:
// two rows sharing a source run ZERO weighted searches (they share one
// memoized tree instead); reverting the nil-reader classification reads 2.
func TestMissingWeightColumnTakesBFS(t *testing.T) {
	ctx := weightedGraph(t)
	sp := &plan.SpStage{
		PathSlot: 2, From: 0, To: 1,
		Dir: graph.Outgoing, Types: []string{"R"},
		Weight: &ast.CostSpec{Kind: ast.CostProperty, Prop: "nosuchweight"},
	}
	before := weightedSearches
	rows := runSPStage(ctx, sp, [][]value.Value{
		{value.Node(0), value.Node(1), value.Null()},
		{value.Node(0), value.Node(2), value.Null()},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Unit weights: min-hop answers -- 0->1 direct (2 nodes), 0->2 (2 nodes).
	for i, r := range rows {
		nodes, _, ok := r[2].AsPath()
		if !ok || len(nodes) != 2 {
			t.Fatalf("row %d path = %v (ok=%v), want the direct min-hop edge", i, nodes, ok)
		}
	}
	if n := weightedSearches - before; n != 0 {
		t.Fatalf("missing-column COST ran %d weighted searches, want 0 (unit weights must take the BFS forms)", n)
	}
	// A REAL property weight still runs the weighted engine.
	before = weightedSearches
	sp.Weight = &ast.CostSpec{Kind: ast.CostProperty, Prop: "w"}
	runSPStage(ctx, sp, [][]value.Value{{value.Node(0), value.Node(1), value.Null()}})
	if n := weightedSearches - before; n != 1 {
		t.Fatalf("real property COST ran %d weighted searches, want 1", n)
	}
}
