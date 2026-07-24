package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestShortestPathExec drives an ANY SHORTEST binding between two bound
// endpoints end-to-end, so buildStageSink constructs the SpStage sink and the
// executor runs the bidirectional search over the chain a0-R->a1-R->a2,
// yielding the two-hop path.
func TestShortestPathExec(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	var ns []graph.NodeID
	for i := 0; i < 3; i++ {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "pid", int64(i))
		ns = append(ns, n)
	}
	for i := 0; i < 2; i++ {
		if _, err := bld.AddRel(ns[i], ns[i+1], "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize("pid"))
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH (s:A {pid: 0}) MATCH (e:A {pid: 2}) MATCH p = ANY SHORTEST (s)-[:R]-{1,4}(e) RETURN p")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("shortest-path rows = %d, want 1", len(rows))
	}
	nodes, _, ok := rows[0][0].AsPath()
	if !ok || len(nodes) != 3 {
		t.Fatalf("shortest path = %v, want 3 nodes (a0-a1-a2)", rows[0][0])
	}
}
