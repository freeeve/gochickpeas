// Dispatch + correctness oracle for the bound-pair named-expand position
// seek (task 143): a cycle-closing hop with a NAMED relationship onto an
// already-bound target is the IC5 closing shape -- the seek must fire (the
// boundPairSeeks counter climbs) instead of enumerating the from-node's
// whole degree, and it must read each parallel relationship's own property
// through its seeked position, preserving multiplicity.
package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// boundSeekFixture: one hub X with a large HAS out-degree (the from-node
// whose enumeration the seek avoids), reaching M then anchor A; the closing
// X-[c:CLOSE]->A edge is doubled with distinct weights, so a correct seek
// yields both parallel relationships' own weights.
func boundSeekFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(256, 256)
	must := func(_ int, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	x, err := b.AddNode("X")
	if err != nil {
		t.Fatal(err)
	}
	m, err := b.AddNode("M")
	if err != nil {
		t.Fatal(err)
	}
	a, err := b.AddNode("A")
	if err != nil {
		t.Fatal(err)
	}
	// X's degree the seek must not walk per row: 200 HAS edges to filler.
	for i := 0; i < 200; i++ {
		f, err := b.AddNode("Filler")
		if err != nil {
			t.Fatal(err)
		}
		must(b.AddRel(x, f, "HAS"))
	}
	must(b.AddRel(x, m, "R1"))
	must(b.AddRel(m, a, "R2"))
	// Two parallel closing edges with distinct weights.
	c1, err := b.AddRel(x, a, "CLOSE")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(c1, "w", int64(10)); err != nil {
		t.Fatal(err)
	}
	c2, err := b.AddRel(x, a, "CLOSE")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(c2, "w", int64(20)); err != nil {
		t.Fatal(err)
	}
	return b.Finalize("boundseek")
}

func TestBoundPairSeekFiresAndReadsRelProps(t *testing.T) {
	g := boundSeekFixture(t)
	q := "MATCH (x:X)-[:R1]->(m:M), (m)-[:R2]->(a:A), (x)-[c:CLOSE]->(a) RETURN c.w AS w ORDER BY w"
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(qq, graph.New(g))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	before := boundPairSeeks
	rows, err := Execute(&eval.Ctx{G: graph.New(g)}, p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Both parallel closing edges survive, each with its own weight.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (parallel closing edges preserve multiplicity)", len(rows))
	}
	w0, _ := rows[0][len(rows[0])-1].AsInt()
	w1, _ := rows[1][len(rows[1])-1].AsInt()
	if w0 != 10 || w1 != 20 {
		t.Fatalf("weights = [%d %d], want [10 20] (each parallel rel read through its own seeked position)", w0, w1)
	}
	if seeks := boundPairSeeks - before; seeks == 0 {
		t.Fatal("boundPairSeeks did not climb: the named bound-target expand fell back to enumeration instead of seeking")
	}
}
