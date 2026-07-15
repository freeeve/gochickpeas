// Multigraph multiplicity consistency (task 144, hazard confirmed live in
// the sibling engine as their 350): parallel relationships must carry
// per-rel multiplicity through EVERY execution form of a hop -- the
// enumerated expand, the bound-target rebind (the semijoin rewrite), and
// the hash-join extraction's probe -- because which form a hop takes is
// the planner's cost-driven choice, and a divergence makes row counts
// plan-dependent. LDBC parity is structurally blind here (simple graph),
// so these pins and the fuzz lane are the coverage.
package gql

import (
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// multiSocialGraph is socialGraph with every 4th relationship duplicated
// (the sibling engine's multigraph fuzz-lane recipe): same reachability,
// parallel-pair multiplicities > 1.
func multiSocialGraph(t testing.TB) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 32)
	people := []struct {
		name string
		age  int64
	}{{"Alice", 30}, {"Bob", 35}, {"Carol", 40}, {"Dave", 25}}
	for _, p := range people {
		id, _ := b.AddNode("Person")
		_ = b.SetProp(id, "name", p.name)
		_ = b.SetProp(id, "age", p.age)
	}
	for _, name := range []string{"Acme", "Globex"} {
		id, _ := b.AddNode("Company")
		_ = b.SetProp(id, "name", name)
	}
	rels := []struct {
		u, v chickpeas.NodeID
		t    string
	}{
		{0, 1, "KNOWS"}, {0, 2, "KNOWS"}, {1, 2, "KNOWS"}, {1, 3, "KNOWS"},
		{2, 3, "KNOWS"}, {2, 1, "KNOWS"}, {3, 0, "KNOWS"},
		{0, 4, "WORKS_AT"}, {1, 4, "WORKS_AT"}, {2, 5, "WORKS_AT"},
	}
	for i, r := range rels {
		if _, err := b.AddRel(r.u, r.v, r.t); err != nil {
			t.Fatal(err)
		}
		if i%4 == 0 {
			if _, err := b.AddRel(r.u, r.v, r.t); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize("multisocial")
}

// TestParallelRelMultiplicityConsistency pins the execution forms of one
// hop over a doubled edge to the same per-rel count: enumerated (unnamed
// and named), comma-joined, and the cycle-closing shape whose rebind runs
// as the semijoin rewrite (the form that used to collapse -- reverting the
// semijoin's multiplicity emission turns exactly the last case red).
func TestParallelRelMultiplicityConsistency(t *testing.T) {
	b := chickpeas.NewBuilder(4, 8)
	a, _ := b.AddNode("A")
	bb, _ := b.AddNode("B")
	b.AddRel(a, bb, "R")
	b.AddRel(a, bb, "R")
	g := b.Finalize("par")
	for _, q := range []string{
		"MATCH (x:A)-[:R]->(y:B) RETURN count(*) AS n",
		"MATCH (x:A)-[r:R]->(y:B) RETURN count(*) AS n",
		"MATCH (x:A), (y:B), (x)-[:R]->(y) RETURN count(*) AS n",
	} {
		if n := oneCount(t, g, q); n != 2 {
			t.Fatalf("count = %d, want 2 (per-rel multiplicity): %s", n, q)
		}
	}
	// The semijoin shape: a cycle-close onto an anchor (both endpoints
	// bound by the chain, unnamed rel -> buildSemijoins rewrites it).
	b2 := chickpeas.NewBuilder(4, 8)
	x, _ := b2.AddNode("X")
	m, _ := b2.AddNode("M")
	an, _ := b2.AddNode("AN")
	b2.AddRel(x, m, "R1")
	b2.AddRel(m, an, "R2")
	b2.AddRel(x, an, "R3")
	b2.AddRel(x, an, "R3") // parallel closing rel
	g2 := b2.Finalize("parclose")
	q := "MATCH (x:X)-[:R1]->(m:M), (m)-[:R2]->(a:AN), (x)-[:R3]->(a) RETURN count(*) AS n"
	if n := oneCount(t, g2, q); n != 2 {
		t.Fatalf("semijoin close count = %d, want 2 (per-rel multiplicity)", n)
	}
	// Plan-shape guard (task 152): the count above only defends the semijoin's
	// per-rel multiplicity if the close actually TAKES the rebind-semijoin
	// rewrite. Without this, a planner regression to an enumerated/reversed or
	// kernel arrangement -- which still counts 2 -- would leave the test green
	// while no longer exercising the semijoin emission at all. Assert the plan
	// carries the [into bound] marker so such a regression turns this red.
	ex, err := Explain(g2, q)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(ex, "[into bound]") {
		t.Fatalf("semijoin close no longer plans as a rebind semijoin -- [into bound] absent, so count 2 stops exercising the semijoin multiplicity path:\n%s", ex)
	}
}

// oneCount runs q through both eval paths and returns its single count.
func oneCount(t *testing.T, g *chickpeas.Snapshot, q string) int64 {
	t.Helper()
	rows := runBoth(t, g, q)
	r, ok := rows.Next()
	if !ok {
		t.Fatalf("no row: %s", q)
	}
	v, _ := r.GetAt(0)
	n, _ := v.AsInt()
	return n
}
