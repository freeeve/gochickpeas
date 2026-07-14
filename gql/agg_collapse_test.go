// Soundness oracle for the aggregate endpoint-collapse gate (task 092): a
// bounded var-expand binding no relationship may bind each endpoint once instead
// of once per trail ONLY when the projection cannot see a duplicate row. This is
// invisible to a planner differential (both planners run the collapse), so the
// oracle here is hand-computed truth over a fixture with KNOWN path multiplicity
// -- the un-collapsed count/sum are the specification, encoded as expected
// values. A collapse wrongly applied to a multiplicity-sensitive aggregate would
// return a demonstrably wrong number.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// trailGraph: Alice(0)->B1(1), Alice(0)->B2(2), B1->C(3), B2->C(3). So
// (Alice)-[:KNOWS]->{1,2}(f) enumerates trail endpoints [B1, B2, C, C]:
// 4 trails, 3 distinct endpoints, C reached by two trails.
func trailGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	var ids []chickpeas.NodeID
	for i := range 4 {
		n, err := b.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "pid", int64(i)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n)
	}
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}} {
		if _, err := b.AddRel(ids[e[0]], ids[e[1]], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("pid")
}

func TestAggCollapseSoundness(t *testing.T) {
	g := trailGraph(t)
	base := "MATCH (p:Person {pid: 0})-[:KNOWS]->{1,2}(f:Person) RETURN "
	one := func(q, col string) int64 {
		got := intCol(t, g, q, col)
		if len(got) != 1 {
			t.Fatalf("%s: %d rows, want 1", q, len(got))
		}
		return got[0]
	}

	// Multiplicity-insensitive: the collapse fires (pinned in the plan test) and
	// the answer equals the un-collapsed truth -- so the collapse is verified
	// sound, not merely fast.
	if n := one(base+"count(DISTINCT f) AS n", "n"); n != 3 {
		t.Errorf("count(DISTINCT f) = %d, want 3 distinct endpoints", n)
	}
	if s := one(base+"sum(DISTINCT f.pid) AS s", "s"); s != 6 {
		t.Errorf("sum(DISTINCT f.pid) = %d, want 6 (1+2+3)", s)
	}
	if m := one(base+"min(f.pid) AS m", "m"); m != 1 {
		t.Errorf("min(f.pid) = %d, want 1", m)
	}
	if m := one(base+"max(f.pid) AS m", "m"); m != 3 {
		t.Errorf("max(f.pid) = %d, want 3", m)
	}

	// Multiplicity-SENSITIVE: must NOT collapse -- each trail row counts, so C
	// (reached twice) contributes twice. If the gate ever wrongly widened to
	// collapse these, count(*) would read 3 and sum 6. This is the mutation
	// guard: it fails the instant the gate admits an unsound aggregate.
	if n := one(base+"count(*) AS n", "n"); n != 4 {
		t.Errorf("count(*) = %d, want 4 trail rows (C reached twice) -- a collapse here is unsound", n)
	}
	if s := one(base+"sum(f.pid) AS s", "s"); s != 9 {
		t.Errorf("sum(f.pid) = %d, want 9 (1+2+3+3) -- a collapse here is unsound", s)
	}
}
