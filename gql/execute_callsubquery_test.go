package gql

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// CALL subquery execution tests: correlated and uncorrelated (cross-join)
// forms, multi-variable and UNION-branch scope clauses, the explicit empty
// scope, and the BI Q4 miniature (collected list -> FOR-driven UNION ALL ->
// outer aggregation). Split from execute_agg_test.go (which keeps the
// shared fixtures/helpers and the aggregation tests).

func TestCallSubqueryCorrelated(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS friends } RETURN p.name AS name, friends ORDER BY name")
	want := map[string]int64{"Alice": 2, "Bob": 2, "Carol": 2, "Dave": 1}
	n := 0
	for r := range rows.All() {
		n++
		nv, _ := r.Get("name")
		name, _ := nv.AsStr()
		fv, _ := r.Get("friends")
		f, _ := fv.AsInt()
		if f != want[name] {
			t.Fatalf("%s friends = %d, want %d", name, f, want[name])
		}
	}
	if n != 4 {
		t.Fatalf("rows = %d", n)
	}
}

func TestCallSubqueryUncorrelatedCrossJoin(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (c:Company) CALL { MATCH (p:Person {name: 'Alice'}) RETURN p.name AS a } RETURN c.name AS cn, a ORDER BY cn")
	batch := rows.NextBatch(10)
	if len(batch) != 2 {
		t.Fatalf("cross-join rows = %d", len(batch))
	}
	for _, r := range batch {
		if v, _ := r.Get("a"); func() string { s, _ := v.AsStr(); return s }() != "Alice" {
			t.Fatalf("a = %v", v)
		}
	}
	// A non-matching inner subquery drops the outer row (inner join).
	rows = runBoth(t, g,
		"MATCH (c:Company) CALL { MATCH (p:Person {name: 'Zed'}) RETURN p.name AS a } RETURN c.name AS cn")
	if _, ok := rows.Next(); ok {
		t.Fatal("empty subquery drops outer rows")
	}
}

// TestCallSubqueryScopeMultiVar pins a scope clause importing more than
// one variable: both cross the boundary and are readable in the body.
func TestCallSubqueryScopeMultiVar(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person)-[:WORKS_AT]->(c:Company) CALL (p, c) { RETURN p.name || '@' || c.name AS tag } RETURN tag ORDER BY tag")
	got := strRows(t, rows, "tag")
	want := []string{"Alice@Acme", "Bob@Acme", "Carol@Globex"}
	if len(got) != len(want) {
		t.Fatalf("tags = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags = %v, want %v", got, want)
		}
	}
}

// TestCallSubqueryScopeUnionBranches pins that one scope clause is
// authoritative for every UNION branch of the body: both branches read
// the import without any per-branch declaration.
func TestCallSubqueryScopeUnionBranches(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person {name: 'Alice'}) CALL (p) { "+
			"MATCH (p)-[:KNOWS]->(f) RETURN f.name AS x "+
			"UNION ALL MATCH (p)-[:WORKS_AT]->(c) RETURN c.name AS x } "+
			"RETURN x ORDER BY x")
	got := strRows(t, rows, "x")
	want := []string{"Acme", "Bob", "Carol"}
	if len(got) != len(want) {
		t.Fatalf("branch rows = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("branch rows = %v, want %v", got, want)
		}
	}
}

// TestCallSubqueryEmptyScope pins CALL () {} as an explicit empty import
// set: the body runs uncorrelated exactly like the no-scope form, and an
// outer variable referenced inside is a bind error, never silently
// imported.
func TestCallSubqueryEmptyScope(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (c:Company) CALL () { MATCH (p:Person {name: 'Alice'}) RETURN p.name AS a } RETURN c.name AS cn, a ORDER BY cn")
	if n := len(rows.NextBatch(10)); n != 2 {
		t.Fatalf("empty-scope cross join rows = %d, want 2", n)
	}
	if _, err := Run(g, "MATCH (p:Person) CALL () { RETURN p.age AS a } RETURN a"); err == nil {
		t.Fatal("outer variable inside CALL () {} must be a bind error")
	}
}

// TestCallSubqueryQ4Miniature drives the BI Q4 shape end-to-end: a
// collected list crosses into the CALL via the scope clause, FOR-driven
// UNION ALL branches expand it, and the outer query aggregates the union.
func TestCallSubqueryQ4Miniature(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person) WHERE p.age >= 30 RETURN collect(p.name) AS names "+
			"NEXT CALL (names) { "+
			"FOR n IN names MATCH (q:Person {name: n})-[:KNOWS]->(f) RETURN f.name AS fn "+
			"UNION ALL FOR n IN names RETURN n AS fn } "+
			"RETURN count(fn) AS total")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no row")
	}
	// names = [Alice, Bob, Carol]; branch 1 yields each of their 2 KNOWS
	// targets (6 rows), branch 2 replays the list (3 rows).
	if v, _ := r.Get("total"); !value.Equal(v, value.Int(9)) {
		t.Fatalf("total = %v, want 9", v)
	}
}
