// GPML batch semantics (task 123): inline element predicates desugar to
// the clause WHERE, `?` is the {0,1} quantifier, `%` is the any-label
// wildcard, and REPEATABLE ELEMENTS switches a clause to walk semantics
// (relationship reuse allowed) where the TRAIL default excludes it.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func gpmlFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mk := func(label string, age int64) chickpeas.NodeID {
		n, err := b.AddNode(label)
		must(err)
		must(b.SetProp(n, "age", age))
		return n
	}
	a := mk("A", 30)
	bb := mk("B", 25)
	mk("B", 35)
	_, err := b.AddRel(a, bb, "R")
	must(err)
	return b.Finalize("gpml")
}

func countOf(t *testing.T, g *chickpeas.Snapshot, q string) int64 {
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

func TestElementPatternWhere(t *testing.T) {
	g := gpmlFixture(t)
	// Node predicate; may reference other clause variables (ISO scoping).
	if n := countOf(t, g, "MATCH (x:B WHERE x.age > 30) RETURN count(*) AS n"); n != 1 {
		t.Fatalf("node WHERE = %d, want 1", n)
	}
	if n := countOf(t, g, "MATCH (a:A)-[:R]->(x:B WHERE x.age < a.age) RETURN count(*) AS n"); n != 1 {
		t.Fatalf("cross-var node WHERE = %d, want 1", n)
	}
	// Rel predicate.
	if n := countOf(t, g, "MATCH (a:A)-[r:R WHERE a.age > 0]->(x) RETURN count(*) AS n"); n != 1 {
		t.Fatalf("rel WHERE = %d, want 1", n)
	}
	// Combines with a clause WHERE (both apply).
	if n := countOf(t, g, "MATCH (x:B WHERE x.age > 20) WHERE x.age < 30 RETURN count(*) AS n"); n != 1 {
		t.Fatalf("combined WHERE = %d, want 1", n)
	}
}

func TestQuestionQuantifier(t *testing.T) {
	g := gpmlFixture(t)
	// {0,1}: A itself (0 hops) plus its one R-neighbor.
	if n := countOf(t, g, "MATCH (a:A)-[:R]->?(x) RETURN count(*) AS n"); n != 2 {
		t.Fatalf("? quantifier = %d, want 2 (self + one hop)", n)
	}
}

func TestLabelWildcard(t *testing.T) {
	g := gpmlFixture(t)
	if n := countOf(t, g, "MATCH (x:%) RETURN count(*) AS n"); n != 3 {
		t.Fatalf("%% = %d, want 3 (every node here is labeled)", n)
	}
	if n := countOf(t, g, "MATCH (x:%&!A) RETURN count(*) AS n"); n != 2 {
		t.Fatalf("%%&!A = %d, want 2", n)
	}
}

func TestRepeatableElementsWalk(t *testing.T) {
	g := gpmlFixture(t)
	// One undirected R edge: a 2-hop undirected pattern must reuse it, so
	// TRAIL (the default) finds nothing and REPEATABLE ELEMENTS finds the
	// two back-and-forth walks (a-b-a and b-a-b).
	base := "(x)-[:R]-(y)-[:R]-(z) RETURN count(*) AS n"
	if n := countOf(t, g, "MATCH "+base); n != 0 {
		t.Fatalf("trail default = %d, want 0 (single edge cannot repeat)", n)
	}
	if n := countOf(t, g, "MATCH REPEATABLE ELEMENTS "+base); n != 2 {
		t.Fatalf("repeatable = %d, want 2 (x-y-x both directions)", n)
	}
	// DIFFERENT EDGES parses and keeps the default.
	if n := countOf(t, g, "MATCH DIFFERENT EDGES "+base); n != 0 {
		t.Fatalf("different edges = %d, want 0", n)
	}
}
