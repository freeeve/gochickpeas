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
