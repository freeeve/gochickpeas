package gql_test

import (
	"github.com/freeeve/gochickpeas/gql"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestOptionalNullPostFilterNotWronglyPromoted locks the anti-join-shaped
// query semantics against the ragedb bug class (task 217): an IS NULL post-
// filter over an OPTIONAL MATCH must distinguish the optional's NEWLY-bound
// variable from a pre-bound anchor. `b IS NULL` (b introduced by the
// optional) is the genuine anti-join -- persons with no KNOWS edge. `a IS
// NULL` (a bound by the required match) is a contradiction -- a is never
// null, so the result is empty. gochickpeas plans a literal post-filter over
// the optional expand (no anti-join promotion), so both are correct; this
// pins them so a future promotion optimization cannot silently regress the
// pre-bound-anchor case.
func TestOptionalNullPostFilterNotWronglyPromoted(t *testing.T) {
	// a0 -KNOWS-> a1; a1 and a2 have no outgoing KNOWS. All are Person.
	b := chickpeas.NewBuilder(8, 8)
	a0, _ := b.AddNode("Person")
	a1, _ := b.AddNode("Person")
	a2, _ := b.AddNode("Person")
	if _, err := b.AddRel(a0, a1, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize("antijoin217")

	anchorIDs := func(src string) map[chickpeas.NodeID]bool {
		t.Helper()
		rows, err := gql.RunUncached(g, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		got := map[chickpeas.NodeID]bool{}
		for r := range rows.All() {
			v, _ := r.GetAt(0)
			id, ok := v.AsNode()
			if !ok {
				t.Fatalf("%s: column a is not a node: %+v", src, v)
			}
			got[id] = true
		}
		return got
	}

	// Genuine anti-join: b (optional-introduced) IS NULL -> persons with no
	// outgoing KNOWS = {a1, a2}, and NOT a0 (which has one).
	for _, src := range []string{
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER b IS NULL RETURN a",
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, b NEXT FILTER b IS NULL RETURN a",
	} {
		got := anchorIDs(src)
		if len(got) != 2 || !got[a1] || !got[a2] || got[a0] {
			t.Fatalf("%s: got %v, want {a1,a2} only", src, got)
		}
	}

	// Contradiction: a (pre-bound anchor) IS NULL -> a is never null, so the
	// result is EMPTY. The ragedb bug wrongly returned the anti-join set here.
	for _, src := range []string{
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER a IS NULL RETURN a",
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, b NEXT FILTER a IS NULL RETURN a",
	} {
		if got := anchorIDs(src); len(got) != 0 {
			t.Fatalf("%s: got %v, want empty (pre-bound anchor is never null)", src, got)
		}
	}

	// Null tests THROUGH NOT (task 239, the ragedb null-rejection-recursion
	// class): NOT(b IS NOT NULL) is TRUE on the null-extended rows -- it must
	// keep exactly the anti-join set, never license treating the optional as
	// required.
	for _, tc := range []struct {
		src  string
		want []chickpeas.NodeID
	}{
		{"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER NOT (b IS NOT NULL) RETURN a", []chickpeas.NodeID{a1, a2}},
		{"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER NOT (NOT (b IS NULL)) RETURN a", []chickpeas.NodeID{a1, a2}},
		{"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER NOT (b IS NULL) RETURN a", []chickpeas.NodeID{a0}},
		{"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, b NEXT FILTER NOT (b IS NOT NULL) RETURN a", []chickpeas.NodeID{a1, a2}},
	} {
		got := anchorIDs(tc.src)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.src, got, tc.want)
		}
		for _, id := range tc.want {
			if !got[id] {
				t.Fatalf("%s: got %v, want %v", tc.src, got, tc.want)
			}
		}
	}
}

// TestOptionalComparisonThroughNot pins three-valued NOT over a comparison
// on an optional variable (task 239): NOT flips FALSE to TRUE but leaves
// NULL as NULL, so NOT(b.x = wrong) keeps only bound rows whose comparison
// was genuinely false -- a null-extended b (comparison NULL) stays dropped
// on both sides of the NOT.
func TestOptionalComparisonThroughNot(t *testing.T) {
	// a0 -KNOWS-> a1 (a1 has x=1); a1, a2 have no outgoing KNOWS.
	b := chickpeas.NewBuilder(8, 8)
	a0, _ := b.AddNode("Person")
	a1, _ := b.AddNode("Person")
	if _, err := b.AddNode("Person"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(a1, "x", int64(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(a0, a1, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize("not239")

	count := func(src string) int {
		t.Helper()
		rows, err := gql.RunUncached(g, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		n := 0
		for range rows.All() {
			n++
		}
		return n
	}
	// b.x = 1 is TRUE only on a0's bound row: NOT drops it and the null
	// rows stay null -> empty.
	if n := count("MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER NOT (b.x = 1) RETURN a"); n != 0 {
		t.Fatalf("NOT(true-comparison) rows = %d, want 0", n)
	}
	// b.x = 2 is FALSE on the bound row (NOT -> TRUE keeps a0) and NULL on
	// the null-extended rows (NOT -> NULL keeps dropping them).
	if n := count("MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) FILTER NOT (b.x = 2) RETURN a"); n != 1 {
		t.Fatalf("NOT(false-comparison) rows = %d, want 1", n)
	}
}
