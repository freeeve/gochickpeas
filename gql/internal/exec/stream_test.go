package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestRowArena covers the bump-allocating row arena: copyRow retains a
// copy (not an alias) of a transient row, successive rows do not share
// backing, and rollback frees the most recent alloc so the next copyRow
// reuses that slot.
func TestRowArena(t *testing.T) {
	a := &rowArena{width: 3}

	src := []value.Value{value.Int(1), value.Int(2), value.Int(3)}
	r1 := a.copyRow(src)
	if len(r1) != 3 {
		t.Fatalf("copied row width = %d, want 3", len(r1))
	}
	// Mutating the source after the copy must not change the retained row.
	src[0] = value.Int(99)
	if v, _ := r1[0].AsInt(); v != 1 {
		t.Fatal("copyRow must copy, not alias, the source row")
	}

	// A second retained row does not share backing with the first.
	r2 := a.copyRow([]value.Value{value.Int(4), value.Int(5), value.Int(6)})
	r2[0] = value.Int(0)
	if v, _ := r1[0].AsInt(); v != 1 {
		t.Fatal("distinct arena rows must not share backing")
	}

	// rollback releases r2's slot; the next copyRow reuses it, so r2 (still
	// pointing at that slot) now reads the reused row's values.
	a.rollback()
	r3 := a.copyRow([]value.Value{value.Int(7), value.Int(8), value.Int(9)})
	if v, _ := r3[0].AsInt(); v != 7 {
		t.Fatalf("reused row = %v, want 7", r3[0])
	}
	if v, _ := r2[0].AsInt(); v != 7 {
		t.Fatal("rollback must free the slot for the next alloc to reuse")
	}
}

// TestCallSubqueryExec drives the CALL {} lateral-join sinks: a correlated
// subquery joins each outer row with its own inner match, and an
// uncorrelated one crosses every outer row with a shared inner result.
func TestCallSubqueryExec(t *testing.T) {
	// a0(A)-R->x0, a1(A)-R->x1 (ids 0,1,2,3).
	bld := chickpeas.NewBuilder(8, 8)
	a0, _ := bld.AddNode("A")
	x0, _ := bld.AddNode("X")
	if _, err := bld.AddRel(a0, x0, "R"); err != nil {
		t.Fatal(err)
	}
	a1, _ := bld.AddNode("A")
	x1, _ := bld.AddNode("X")
	if _, err := bld.AddRel(a1, x1, "R"); err != nil {
		t.Fatal(err)
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	run := func(src string) [][]value.Value {
		t.Helper()
		q, err := parser.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatalf("plan %q: %v", src, err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatalf("exec %q: %v", src, err)
		}
		return rows
	}

	// Correlated: each A joins to its own R-neighbor -> (a0,x0), (a1,x1).
	rows := run("MATCH (a:A) CALL (a) { MATCH (a)-[:R]->(x) RETURN x AS n } RETURN a, n")
	pairs := map[[2]graph.NodeID]bool{}
	for _, r := range rows {
		av, _ := r[0].AsNode()
		nv, _ := r[1].AsNode()
		pairs[[2]graph.NodeID{av, nv}] = true
	}
	if len(pairs) != 2 || !pairs[[2]graph.NodeID{a0, x0}] || !pairs[[2]graph.NodeID{a1, x1}] {
		t.Fatalf("correlated CALL pairs = %v, want {(a0,x0),(a1,x1)}", pairs)
	}

	// Uncorrelated: every A is crossed with the shared count of X (=2).
	rows = run("MATCH (a:A) CALL { MATCH (x:X) RETURN count(x) AS c } RETURN a, c")
	if len(rows) != 2 {
		t.Fatalf("uncorrelated CALL rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if c, _ := r[1].AsInt(); c != 2 {
			t.Fatalf("uncorrelated count = %d, want 2", c)
		}
	}
}

// TestNamedPathExec drives a named-path binding end-to-end so the match sink
// assembles the path value (stream.forward's PathBind branch): MATCH p =
// (a)-[:R]->(b) yields one path whose node and relationship sequences match
// the traversed edge.
func TestNamedPathExec(t *testing.T) {
	bld := chickpeas.NewBuilder(4, 4)
	a0, _ := bld.AddNode("A")
	a1, _ := bld.AddNode("A")
	if _, err := bld.AddRel(a0, a1, "R"); err != nil {
		t.Fatal(err)
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH p = (a:A)-[:R]->(b:A) RETURN p")
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
		t.Fatalf("named-path rows = %d, want 1", len(rows))
	}

	nodes, rels, ok := rows[0][0].AsPath()
	if !ok {
		t.Fatalf("RETURN p = %v, want a path value", rows[0][0])
	}
	if len(nodes) != 2 || uint32(nodes[0]) != uint32(a0) || uint32(nodes[1]) != uint32(a1) {
		t.Fatalf("path nodes = %v, want [a0 a1]", nodes)
	}
	if len(rels) != 1 {
		t.Fatalf("path rels = %v, want exactly 1", rels)
	}
}
