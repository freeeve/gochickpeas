// Duplicate output-column rejection (task 148): a projection may not return
// two columns with the same name -- the result table cannot carry duplicate
// names and a later ORDER BY / reference to the name is ambiguous (the fuzz
// oracle caught this as an ORDER BY invariant violation on
// `RETURN c AS n, count(0) AS n ORDER BY n`). An explicit item shadows a
// same-named `*` variable rather than duplicating it, so a re-binding LET
// stays legal.
package gql

import (
	"errors"
	"testing"
)

func TestDuplicateOutputColumnRejected(t *testing.T) {
	g := socialGraph(t)
	// Each of these has two columns resolving to the same output name.
	reject := []string{
		"MATCH (n:Person) RETURN n.name AS x, n.age AS x",       // two explicit aliases
		"MATCH (n:Person) RETURN n.name, n.name",                // two derived names
		"MATCH (n:Person) RETURN n.age AS a, n.name AS a",       // agg-free duplicate
		"MATCH (c)--() RETURN c AS n, count(0) AS n ORDER BY n", // the fuzz repro (task 148)
		"MATCH (n:Person) RETURN n AS m, n.age AS m",            // node and scalar collide
	}
	for _, q := range reject {
		if _, err := Run(g, q); !errors.Is(err, ErrBind) {
			t.Fatalf("want ErrBind for duplicate output column: %q -> %v", q, err)
		}
	}
	// Distinct names, and shadowing/re-binding forms, stay legal.
	accept := []string{
		"MATCH (n:Person) RETURN n.name AS a, n.age AS b",
		"MATCH (n:Person) RETURN count(*) AS n, avg(n.age) AS m",
		"MATCH (n:Person) LET n = 5 RETURN n",        // LET re-bind: explicit shadows the * var
		"MATCH (n:Person) RETURN *, n.name AS extra", // * plus a distinct explicit column
	}
	for _, q := range accept {
		if _, err := Run(g, q); err != nil {
			t.Fatalf("unexpected rejection of a legal projection: %q -> %v", q, err)
		}
	}
}

// TestExplicitShadowsStarColumn pins the shadowing semantics the fix relies
// on: an explicit item re-projecting a `*` variable under the same name
// yields a SINGLE column carrying the explicit value, not two columns.
func TestExplicitShadowsStarColumn(t *testing.T) {
	g := socialGraph(t)
	rows, err := Run(g, "MATCH (n:Person) RETURN *, 7 AS n ORDER BY n")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cols := rows.Columns()
	seen := 0
	for _, c := range cols {
		if c == "n" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("column n appears %d times, want 1 (explicit item shadows the * variable): %v", seen, cols)
	}
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no rows")
	}
	idx := -1
	for i, c := range cols {
		if c == "n" {
			idx = i
		}
	}
	v, _ := r.GetAt(idx)
	if iv, ok := v.AsInt(); !ok || iv != 7 {
		t.Fatalf("column n = %v, want the explicit 7 (not the * node)", v.Kind())
	}
}
