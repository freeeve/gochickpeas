package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestTriangleCountNoFalseSymmetry locks triangle count(*) against the ragedb
// bug class (task 220): a triangle-symmetry optimization that multiplies a
// min-anchored count by 3 must fire ONLY on a directed 3-cycle (each vertex
// in=out=1), never on a merely pairwise-connected triangle. gochickpeas has
// no such symmetry-breaking optimization -- it counts actual matches -- so
// both a directed 3-cycle and a transitive triangle count correctly; this
// pins them so a future symmetry optimization cannot misapply the factor.
func TestTriangleCountNoFalseSymmetry(t *testing.T) {
	// Two disjoint structures: a directed 3-cycle 0->1->2->0, and a
	// transitive triangle 3->4, 4->5, 3->5 (pairwise-connected, no
	// automorphism: 3 is the source of two, 5 the sink of two).
	b := chickpeas.NewBuilder(8, 8)
	n := make([]chickpeas.NodeID, 6)
	for i := range n {
		v, err := b.AddNode("V")
		if err != nil {
			t.Fatal(err)
		}
		n[i] = v
	}
	edges := [][2]int{{0, 1}, {1, 2}, {2, 0}, {3, 4}, {4, 5}, {3, 5}}
	for _, e := range edges {
		if _, err := b.AddRel(n[e[0]], n[e[1]], "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("triangle220")

	count := func(src string) int64 {
		t.Helper()
		rows, err := RunUncached(g, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("%s: no row", src)
		}
		v, _ := r.GetAt(0)
		iv, ok := v.AsInt()
		if !ok {
			t.Fatalf("%s: count not an int: %+v", src, v)
		}
		return iv
	}

	// The directed-3-cycle pattern matches the C3 once per rotation -> 3,
	// and does not match the transitive triangle (it is not a cycle).
	if got := count("MATCH (a)-[:R]->(b), (b)-[:R]->(c), (c)-[:R]->(a) RETURN count(*) AS n"); got != 3 {
		t.Fatalf("directed-3-cycle count = %d, want 3", got)
	}
	// The transitive pattern matches the transitive triangle exactly once
	// (no automorphism) and does not match the C3 -> 1. This is the case a
	// false factor-3 symmetry would overcount.
	if got := count("MATCH (a)-[:R]->(b), (b)-[:R]->(c), (a)-[:R]->(c) RETURN count(*) AS n"); got != 1 {
		t.Fatalf("transitive-triangle count = %d, want 1 (no false symmetry factor)", got)
	}
}
