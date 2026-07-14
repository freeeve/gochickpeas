package main

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// smallPersonGraph builds a tiny KNOWS chain over Person nodes carrying age
// and name, enough for a planner to face several candidate anchors and a
// filter -- the shape where a map-order-dependent anchor choice would surface.
func smallPersonGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(64, 64)
	var ids []chickpeas.NodeID
	for i := range 20 {
		n, err := b.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "age", int64(18+i)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "name", fmt.Sprintf("p%02d", i)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n)
	}
	for i := range len(ids) - 1 {
		if _, err := b.AddRel(ids[i], ids[i+1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize()
}

// TestPlanDistinctStable pins the planner as deterministic across repeated
// in-process plannings of one query (task 090). Go randomizes map iteration
// order on every run by design, so any planner decision that leaks map order
// would produce more than one distinct plan text here -- the property the
// harness's -plan-stability mode certifies before trusting a timing. A single
// distinct plan across many plannings is the pass condition; this locks it in
// as a regression so a future map-order-dependent planner change fails loudly
// rather than surfacing as a phantom timing anomaly on a shared box.
func TestPlanDistinctStable(t *testing.T) {
	g := smallPersonGraph(t)
	const query = `MATCH (p:Person)-[:KNOWS]->(f:Person)
		WHERE p.age > 20 AND f.age < 35
		RETURN f.name ORDER BY f.name LIMIT 5`

	distinct, err := planDistinct(g, query, 50)
	if err != nil {
		t.Fatalf("planDistinct: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("planner nondeterministic: %d distinct plans across 50 plannings, want 1", distinct)
	}
}

// TestPlanDistinctReportsCount confirms planDistinct plans exactly the query it
// is given and returns a positive distinct count (never zero) for a valid plan,
// so the harness's >1 test has a sound denominator.
func TestPlanDistinctReportsCount(t *testing.T) {
	g := smallPersonGraph(t)
	distinct, err := planDistinct(g, `MATCH (p:Person) WHERE p.age > 25 RETURN p.name`, 8)
	if err != nil {
		t.Fatalf("planDistinct: %v", err)
	}
	if distinct < 1 {
		t.Fatalf("distinct = %d, want >= 1", distinct)
	}
}

// TestGoldenRoundTrip: a captured golden must parse back to exactly the plans
// it was built from, including multi-line plans with blank interior lines, so a
// capture-then-verify on unchanged plans never spuriously drifts.
func TestGoldenRoundTrip(t *testing.T) {
	entries := []goldenEntry{
		{id: "SPB/q1", plan: "NodeScan (a:Tag)\n  [anchor: a:Tag card=N]\nProject [a.name]"},
		{id: "BI/Q8", plan: "NodeByProperty (p:Person {id = N})\nExpand (p)-[:KNOWS]->(f)\nAggregate (group=[x]; count(f))"},
		{id: "IC/3", plan: "NodeScan (n:City)\nProject [n.name]"},
	}
	got := parseGolden(formatGolden(entries))
	if len(got) != len(entries) {
		t.Fatalf("round-trip lost entries: got %d, want %d", len(got), len(entries))
	}
	for _, e := range entries {
		if got[e.id] != e.plan {
			t.Fatalf("round-trip mismatch for %s:\n got %q\nwant %q", e.id, got[e.id], e.plan)
		}
	}
}

// TestDiffGoldenDetectsDrift: the diff must flag a changed plan, a new query,
// and a query that vanished -- and stay silent when every plan is identical.
func TestDiffGoldenDetectsDrift(t *testing.T) {
	base := []goldenEntry{
		{id: "A", plan: "NodeScan (a)\nProject [a]"},
		{id: "B", plan: "NodeScan (b)\nProject [b]"},
	}
	golden := parseGolden(formatGolden(base))

	if d := diffGolden(golden, base); len(d) != 0 {
		t.Fatalf("identical plans reported drift: %v", d)
	}

	changed := []goldenEntry{
		{id: "A", plan: "NodeByProperty (a {k = N})\nProject [a]"}, // A's plan moved
		// B missing entirely
		{id: "C", plan: "NodeScan (c)\nProject [c]"}, // C is new
	}
	d := diffGolden(golden, changed)
	joined := fmt.Sprint(d)
	for _, want := range []string{"A: plan shape changed", "C: new query", "B: in golden but absent"} {
		if !contains(d, want) {
			t.Fatalf("expected drift %q in %s", want, joined)
		}
	}
}

// contains reports whether any element of xs has want as a prefix.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if len(x) >= len(want) && x[:len(want)] == want {
			return true
		}
	}
	return false
}
