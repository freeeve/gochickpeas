package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestGroupJoinExecMatchesNested drives the OPTIONAL-MATCH group-join
// decorrelation sink and pins the fast path to the general path: the
// decorrelated aggregate (forced via the outer-rows floor knob) must produce
// exactly the same per-anchor counts as the nested OPTIONAL execution.
// Fixture: p0-KNOWS->p1, p0-KNOWS->p2, p1-KNOWS->p2; p3 has no KNOWS.
func TestGroupJoinExecMatchesNested(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("Person"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		if _, err := bld.AddRel(graph.NodeID(e[0]), graph.NodeID(e[1]), "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, count(b) AS c")
	if err != nil {
		t.Fatal(err)
	}
	counts := func() map[graph.NodeID]int64 {
		t.Helper()
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		out := map[graph.NodeID]int64{}
		for _, r := range rows {
			id, _ := r[0].AsNode()
			c, _ := r[1].AsInt()
			out[id] = c
		}
		return out
	}

	defer func(v float64) { plan.GroupJoinMinOuterRows = v }(plan.GroupJoinMinOuterRows)

	// The nested OPTIONAL execution (the group-join rewrite disabled).
	plan.GroupJoinMinOuterRows = 1e18
	nested := counts()
	// The decorrelated group-join (rewrite forced onto the small fixture).
	plan.GroupJoinMinOuterRows = 0
	gj := counts()

	// The two paths must agree exactly.
	if len(gj) != len(nested) {
		t.Fatalf("group-join result size %d != nested %d", len(gj), len(nested))
	}
	for id, c := range nested {
		if gj[id] != c {
			t.Fatalf("group-join count[%d] = %d, nested = %d", id, gj[id], c)
		}
	}
	// And the counts are the expected KNOWS out-degrees (0 for null-extended).
	for id, want := range map[graph.NodeID]int64{0: 2, 1: 1, 2: 0, 3: 0} {
		if nested[id] != want {
			t.Fatalf("count[%d] = %d, want %d", id, nested[id], want)
		}
	}
}
