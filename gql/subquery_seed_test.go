// Unanchored subquery seed orientation (task 201, ported from the Rust
// sibling's IC12 find): an EXISTS/COUNT inner pattern with NEITHER
// endpoint outer-anchored seeds its walk from the statically stronger
// endpoint -- a property literal beats a bare label beats an unlabeled
// node -- instead of blindly scanning the written start's candidate set.
// Reversal enumerates the same match multiset, and these tests pin the
// two subtle arms: the zero-length quantifier (the seed node itself must
// satisfy BOTH endpoint patterns) and COUNT's path multiplicity.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// seedFixture: x1:X -S-> root(:C {name:'root'}); x2:X -S-> m -S-> root;
// x1 -S-> m (a second path); xz carries BOTH labels X and C with the
// root name (the zero-length arm's witness); plus a plain :N bystander.
func seedFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	node := func(labels ...string) chickpeas.NodeID {
		n, err := b.AddNode(labels...)
		must(err)
		return n
	}
	root := node("C")
	must(b.SetProp(root, "name", "root"))
	x1 := node("X")
	must(b.SetProp(x1, "v", int64(1)))
	x2 := node("X")
	must(b.SetProp(x2, "v", int64(2)))
	m := node("M")
	xz := node("X", "C")
	must(b.SetProp(xz, "name", "root"))
	_, err := b.AddRel(x1, root, "S")
	must(err)
	_, err = b.AddRel(x1, m, "S")
	must(err)
	_, err = b.AddRel(x2, m, "S")
	must(err)
	_, err = b.AddRel(m, root, "S")
	must(err)
	node("N")
	return b.Finalize("seed-orient")
}

// TestUnanchoredCountSeedReversal pins the reversed walk's result: the
// end has a property literal (strength 2) over the start's bare label
// (strength 1), so the walk seeds from the root side. A quantified hop
// counts distinct endpoint bindings (the subquery walk's reachable-set
// semantics -- x1's two routes to root bind one (x1, root) pair), so the
// expected count is 3: (x1, root), (x2, root), and the ZERO-LENGTH arm
// (xz, xz) through the dual-labeled node that satisfies both endpoint
// patterns. The textually reversed spelling seeds the same side without
// the new fallback, so the two spellings agreeing pins orientation
// independence.
func TestUnanchoredCountSeedReversal(t *testing.T) {
	g := seedFixture(t)
	for _, q := range []string{
		"RETURN COUNT { (:X)-[:S]->{0,2}(:C {name: 'root'}) } AS c",
		"RETURN COUNT { (:C {name: 'root'})<-[:S]-{0,2}(:X) } AS c",
	} {
		rows, err := Run(g, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for r := range rows.All() {
			if c, _ := r.Values()[0].AsInt(); c != 3 {
				t.Fatalf("%s: count = %d, want 3 ((x1,root), (x2,root), zero-length (xz,xz))", q, c)
			}
		}
	}
}

// TestUnanchoredExistsBareStart pins the bare-unlabeled start (strength
// 0): the reversed walk must answer the constant EXISTS true for every
// outer row.
func TestUnanchoredExistsBareStart(t *testing.T) {
	g := seedFixture(t)
	rows, err := Run(g, "MATCH (n:N) WHERE EXISTS { ()-[:S]->{0,2}(:C {name: 'root'}) } RETURN count(n) AS k")
	if err != nil {
		t.Fatal(err)
	}
	for r := range rows.All() {
		if k, _ := r.Values()[0].AsInt(); k != 1 {
			t.Fatalf("kept %d rows, want 1 (constant-true EXISTS)", k)
		}
	}
}

// Pattern comprehensions are fenced off from the seed reversal in code
// (evalPatternComp passes allowSeedReverse=false -- a collecting walk's
// list order is enumeration order), but they are not reachable from the
// parsed GQL subset, so the guard has no surface-level test.
