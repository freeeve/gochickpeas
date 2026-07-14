// Public-surface tests for ExplainCanonical (task 112): the golden plan-shape
// entry point cmd/gqlbench consumes (it cannot import gql/internal/explain).
// Asserts the output pins shape/anchor, excludes volatile parts, and is
// deterministic across plannings -- the properties a plan-regression golden
// depends on.
package gql

import (
	"strings"
	"testing"
)

func TestExplainCanonicalPinsShapeExcludesVolatile(t *testing.T) {
	g := socialGraph(t)
	const q = "MATCH (p:Person {name: 'Alice'})-[:KNOWS]->(f:Person) WHERE f.age > 25 RETURN f.name AS n ORDER BY n LIMIT 3"
	c, err := ExplainCanonical(g, q)
	if err != nil {
		t.Fatalf("ExplainCanonical: %v", err)
	}
	// Shape, scan source, and op keywords are pinned.
	for _, want := range []string{"NodeByProperty (p:Person {name = 'Alice'})", "Expand", "Filter (f.age > 25)", "Project [n]", "OrderBy [n]", "Limit 3"} {
		if !strings.Contains(c, want) {
			t.Fatalf("canonical missing %q:\n%s", want, c)
		}
	}
	// Volatile header / timing never leaks.
	for _, bad := range []string{"EXPLAIN", "Planning:"} {
		if strings.Contains(c, bad) {
			t.Fatalf("canonical leaked volatile %q:\n%s", bad, c)
		}
	}
	for _, ln := range strings.Split(c, "\n") {
		if strings.TrimSpace(ln) == "est" {
			t.Fatalf("canonical kept the est header:\n%s", c)
		}
	}
}

// TestExplainCanonicalDeterministic: Go randomizes map iteration each run, so a
// canonical string that varied across plannings would be useless as a golden.
func TestExplainCanonicalDeterministic(t *testing.T) {
	g := socialGraph(t)
	const q = "MATCH (p:Person {name: 'Alice'})-[:KNOWS]->(f:Person) WHERE f.age > 25 RETURN f.name AS n ORDER BY n"
	first, err := ExplainCanonical(g, q)
	if err != nil {
		t.Fatalf("ExplainCanonical: %v", err)
	}
	for range 50 {
		got, err := ExplainCanonical(g, q)
		if err != nil {
			t.Fatalf("ExplainCanonical: %v", err)
		}
		if got != first {
			t.Fatalf("canonical nondeterministic across plannings:\n--- first ---\n%s\n--- got ---\n%s", first, got)
		}
	}
}

// TestExplainCanonicalParseError surfaces a plan/parse error as an error, not a
// partial string -- the gqlbench golden mode must distinguish "cannot plan" from
// "planned to this shape".
func TestExplainCanonicalParseError(t *testing.T) {
	g := socialGraph(t)
	if _, err := ExplainCanonical(g, "MATCH (p:Person) RETURN"); err == nil {
		t.Fatal("expected an error for a malformed query, got nil")
	}
}
