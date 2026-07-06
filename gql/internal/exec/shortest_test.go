// Weighted shortest-path kernel tests, building the SpStage directly
// (the COST clause's e2e coverage lives in the gql package) -- the
// cheapest route must win over the fewest-hop route, per-edge exclusions
// apply, and each CostSpec kind compiles.
package exec

import (
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
