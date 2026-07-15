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

// TestExpressionBatch pins task 124's expression additions through the
// dual-run harness: three-valued XOR, || concatenation (strings and
// lists, never numbers), the IS TRUE/FALSE truth tables (null reads
// false under both, NOT inverts the whole test), and IS TYPED kinds.
func TestExpressionBatch(t *testing.T) {
	g := gpmlFixture(t)
	one := func(q string) string {
		t.Helper()
		rows := runBoth(t, g, "RETURN "+q+" AS x")
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("no row: %s", q)
		}
		v, _ := r.GetAt(0)
		if v.IsNull() {
			return "null"
		}
		if b, ok := v.AsBool(); ok {
			if b {
				return "true"
			}
			return "false"
		}
		if s, ok := v.AsStr(); ok {
			return s
		}
		if l, ok := v.AsList(); ok {
			out := ""
			for _, e := range l {
				i, _ := e.AsInt()
				out += string(rune('0' + i))
			}
			return out
		}
		return "?"
	}
	for q, want := range map[string]string{
		"true XOR false":         "true",
		"true XOR true":          "false",
		"false XOR false":        "false",
		"true XOR null":          "null",
		"null XOR false":         "null",
		"'a' || 'b'":             "ab",
		"[1,2] || [3]":           "123",
		"'a' || null":            "null",
		"1 || 2":                 "null",
		"true IS TRUE":           "true",
		"false IS TRUE":          "false",
		"(null = 1) IS TRUE":     "false",
		"(null = 1) IS NOT TRUE": "true",
		"false IS FALSE":         "true",
		"(null = 1) IS FALSE":    "false",
		"(null = 1) IS UNKNOWN":  "true",
		"1 IS TYPED INTEGER":     "true",
		"1 IS TYPED FLOAT":       "false",
		"1.5 IS TYPED FLOAT":     "true",
		"'s' IS TYPED STRING":    "true",
		"[1] IS TYPED LIST":      "true",
		"1 IS NOT TYPED STRING":  "true",
	} {
		if got := one(q); got != want {
			t.Fatalf("%s = %s, want %s", q, got, want)
		}
	}
}

// TestSetOperations pins EXCEPT / INTERSECT / UNION DISTINCT with
// hand-derived sets over the fixture (A ages {30}, B ages {25, 35}).
func TestSetOperations(t *testing.T) {
	g := gpmlFixture(t)
	vals := func(q string) []int64 {
		t.Helper()
		rows := runBoth(t, g, q)
		var out []int64
		for r := range rows.All() {
			v, _ := r.GetAt(0)
			n, _ := v.AsInt()
			out = append(out, n)
		}
		return out
	}
	// {30,25,35} INTERSECT {25,35} = {25,35}
	got := vals("MATCH (x) RETURN x.age AS a INTERSECT MATCH (b:B) RETURN b.age AS a")
	if len(got) != 2 {
		t.Fatalf("INTERSECT = %v, want 2 rows", got)
	}
	// {30,25,35} EXCEPT {25,35} = {30}
	got = vals("MATCH (x) RETURN x.age AS a EXCEPT MATCH (b:B) RETURN b.age AS a")
	if len(got) != 1 || got[0] != 30 {
		t.Fatalf("EXCEPT = %v, want [30]", got)
	}
	// UNION DISTINCT dedups across branches.
	got = vals("MATCH (b:B) RETURN b.age AS a UNION DISTINCT MATCH (b:B) RETURN b.age AS a")
	if len(got) != 2 {
		t.Fatalf("UNION DISTINCT = %v, want 2 rows", got)
	}
}
