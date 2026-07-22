package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestOptionalUnsatisfiableEdgeNullExtends locks the OPTIONAL-edge
// contradiction semantics against the ragedb bug class (task 219): an
// OPTIONAL edge whose attribute predicate is unsatisfiable (weight > 5 AND
// weight < 2) can never match, so the optional match null-extends every
// anchor -- the result is NON-empty. Only a REQUIRED edge that cannot match
// empties the query. gochickpeas has no edge-attribute contradiction pruner
// (the predicate is a literal always-false filter), so both are correct by
// construction; this pins them so a future contradiction/no-op pruner cannot
// silently empty the optional case.
func TestOptionalUnsatisfiableEdgeNullExtends(t *testing.T) {
	// a0 -KNOWS(w=10)-> a1 -KNOWS(w=1)-> a2. All are N.
	b := chickpeas.NewBuilder(8, 8)
	a0, _ := b.AddNode("N")
	a1, _ := b.AddNode("N")
	a2, _ := b.AddNode("N")
	i0, err := b.AddRel(a0, a1, "KNOWS")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(i0, "weight", 10.0); err != nil {
		t.Fatal(err)
	}
	i1, err := b.AddRel(a1, a2, "KNOWS")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(i1, "weight", 1.0); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize("unsat219")

	anchorIDs := func(src string) map[chickpeas.NodeID]bool {
		t.Helper()
		rows, err := RunUncached(g, src)
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

	all := map[chickpeas.NodeID]bool{a0: true, a1: true, a2: true}
	sameSet := func(got map[chickpeas.NodeID]bool) bool {
		if len(got) != len(all) {
			return false
		}
		for id := range all {
			if !got[id] {
				return false
			}
		}
		return true
	}

	// OPTIONAL + unsatisfiable edge predicate -> null-extends every anchor.
	if got := anchorIDs("MATCH (a:N) OPTIONAL MATCH (a)-[r:KNOWS]->(b) WHERE r.weight > 5 AND r.weight < 2 RETURN a"); !sameSet(got) {
		t.Fatalf("optional unsatisfiable: got %v, want all anchors (null-extended)", got)
	}
	// OPTIONAL + satisfiable predicate -> also every anchor (a0 matches,
	// a1/a2 null-extend).
	if got := anchorIDs("MATCH (a:N) OPTIONAL MATCH (a)-[r:KNOWS]->(b) WHERE r.weight > 5 RETURN a"); !sameSet(got) {
		t.Fatalf("optional satisfiable: got %v, want all anchors", got)
	}
	// REQUIRED + unsatisfiable edge predicate -> empty (a required edge that
	// cannot match does empty the query).
	if got := anchorIDs("MATCH (a:N)-[r:KNOWS]->(b) WHERE r.weight > 5 AND r.weight < 2 RETURN a"); len(got) != 0 {
		t.Fatalf("required unsatisfiable: got %v, want empty", got)
	}
}
