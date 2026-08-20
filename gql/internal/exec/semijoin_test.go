// Build-once oracle for the bound-target rebind semijoin (task 132): a
// cycle-closing hop onto a single-node anchor is the loop-invariant
// membership shape (LDBC IC6/IC10) -- the anchor's reverse-neighbor set
// must materialize ONCE for the whole stage, with every row testing
// membership against it. The assertion is a BUILD COUNT: one set build
// regardless of row count; a per-row rebuild means the memo is dead.
package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// semijoinFixture: one Anchor node; 40 chains x_i-[:R1]->y_i-[:R2]->anchor;
// the even-numbered x_i additionally hold the closing edge x_i-[:R3]->anchor.
func semijoinFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(128, 128)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := b.AddNode("Anchor")
	must(err)
	for i := 0; i < 40; i++ {
		x, err := b.AddNode("P")
		must(err)
		y, err := b.AddNode("P")
		must(err)
		_, err = b.AddRel(x, y, "R1")
		must(err)
		_, err = b.AddRel(y, a, "R2")
		must(err)
		if i%2 == 0 {
			_, err = b.AddRel(x, a, "R3")
			must(err)
		}
	}
	return b.Finalize("semijoin")
}

func TestSemijoinConstantTargetBuildsOnce(t *testing.T) {
	g := semijoinFixture(t)
	q := "MATCH (x:P)-[:R1]->(y:P), (y)-[:R2]->(a:Anchor), (x)-[:R3]->(a) RETURN count(*) AS n"
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(qq, graph.New(g))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	before := semijoinSetBuilds
	rows, err := Execute(&eval.Ctx{G: graph.New(g)}, p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if n, _ := rows[0][len(rows[0])-1].AsInt(); n != 20 {
		t.Fatalf("count = %d, want 20 (even chains close the cycle)", n)
	}
	builds := semijoinSetBuilds - before
	if builds != 1 {
		t.Fatalf("semijoin set builds = %d, want 1 (constant anchor: materialize the neighborhood once, membership per row; 0 means no semijoin planned, >1 means the memo is dead)", builds)
	}
}

// TestSemijoinLookaheadPrune pins the fill-time lookahead (task 205
// round 12): a level whose NEXT op is a rebind semijoin over a bound
// target sweeps doomed candidates out of its buffer before they bind --
// engagement counted, results identical to the un-pruned walk, and the
// prune must not disturb the semijoin's own multiplicity semantics.
func TestSemijoinLookaheadPrune(t *testing.T) {
	b := chickpeas.NewBuilder(64, 128)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := b.AddNode("Anchor")
	must(err)
	for i := 0; i < 10; i++ {
		x, err := b.AddNode("P")
		must(err)
		good, err := b.AddNode("P")
		must(err)
		bad, err := b.AddNode("P")
		must(err)
		_, err = b.AddRel(x, a, "R3")
		must(err)
		_, err = b.AddRel(x, good, "R1")
		must(err)
		_, err = b.AddRel(x, bad, "R1")
		must(err)
		_, err = b.AddRel(good, a, "R2")
		must(err)
	}
	g := b.Finalize()
	// A cycle inside one pattern puts the closing rebind adjacent to the
	// y-level in one stage: y-candidates sweep against a's R2 reverse
	// set at fill, and each x's dead-end y (no R2 edge) never binds.
	q := "MATCH (a:Anchor)<-[:R3]-(x:P)-[:R1]->(y:P)-[:R2]->(a) RETURN count(*) AS n"
	run := func() int64 {
		qq, err := parser.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.Build(qq, graph.New(g))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := Execute(&eval.Ctx{G: graph.New(g)}, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		n, _ := rows[0][0].AsInt()
		return n
	}
	before := semijoinLookaheadPrunes
	pruned := run()
	if semijoinLookaheadPrunes == before {
		t.Fatal("the lookahead never pruned on a qualifying shape")
	}
	disableSemijoinLookahead = true
	defer func() { disableSemijoinLookahead = false }()
	if general := run(); general != pruned {
		t.Fatalf("pruned walk counted %d, general %d", pruned, general)
	}
	if pruned != 10 {
		t.Fatalf("count = %d, want 10 (one good y per x)", pruned)
	}
}
