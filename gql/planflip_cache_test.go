// Flip-aware cache routing: a template whose blind plan differs
// structurally from sighted planning executes through the sighted path
// (results were always identical -- the flip is a COST hazard -- so these
// tests pin the routing and the detection default, and the census
// measures the cost effect).
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// runRows executes f and collects all rows' first columns as value keys.
func runRows(t *testing.T, f func() (*Rows, error)) []string {
	t.Helper()
	rows, err := f()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for r := range rows.All() {
		v, _ := r.GetAt(0)
		out = append(out, value.Key(v))
	}
	return out
}

// TestUnflippedTemplateStaysCached pins the detection default: a plain
// query's blind plan matches its sighted plan, so the template is not
// marked flipped and repeats hit the cache.
func TestUnflippedTemplateStaysCached(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(0)
	q := "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n ORDER BY n"
	want := runRows(t, func() (*Rows, error) { return Run(g, q) })
	got := runRows(t, func() (*Rows, error) { return c.Run(g, q) })
	if len(got) != len(want) {
		t.Fatalf("cached run rows = %d, want %d", len(got), len(want))
	}
	for _, cp := range c.byTemplate {
		if cp.flipped {
			t.Fatal("plain template marked flipped")
		}
	}
	_ = runRows(t, func() (*Rows, error) { return c.Run(g, q) })
	if l1, _, _ := c.stats(); l1 != 1 {
		t.Fatalf("hitsL1 = %d, want 1 (unflipped repeat must use the cache)", l1)
	}
}

// TestFlippedTemplateRoutesSighted pins the routing: with a template
// marked flipped (forced -- constructing a natural flip needs graph-scale
// statistics), both the L1-hit and L2-hit paths return the same rows as
// the uncached sighted path.
func TestFlippedTemplateRoutesSighted(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(0)
	q := "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n ORDER BY n"
	want := runRows(t, func() (*Rows, error) { return Run(g, q) })
	_ = runRows(t, func() (*Rows, error) { return c.Run(g, q) }) // warm
	for _, cp := range c.byTemplate {
		cp.flipped = true
	}
	got := runRows(t, func() (*Rows, error) { return c.Run(g, q) }) // L1 hit, flipped -> sighted
	if len(got) != len(want) {
		t.Fatalf("flipped route rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flipped route row %d = %q, want %q", i, got[i], want[i])
		}
	}
	// A literal variant of the same template (L2 hit) routes sighted too.
	q2 := "MATCH (p:Person) WHERE p.age > 31 RETURN p.name AS n ORDER BY n"
	want2 := runRows(t, func() (*Rows, error) { return Run(g, q2) })
	got2 := runRows(t, func() (*Rows, error) { return c.Run(g, q2) })
	if len(got2) != len(want2) {
		t.Fatalf("flipped L2 route rows = %d, want %d", len(got2), len(want2))
	}
}

// TestFlipDetectionSameTreeNotFlipped pins planFlipped's negative: the
// template plan for a query whose structure survives parameter lifting
// compares equal to the sighted plan.
func TestFlipDetectionSameTreeNotFlipped(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(0)
	queries := []string{
		"MATCH (p:Person {name: 'Alice'}) RETURN p.age AS a",
		"MATCH (p:Person)-[:KNOWS]->(f:Person) RETURN count(*) AS c",
	}
	for _, q := range queries {
		if _, err := c.Run(g, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, cp := range c.byTemplate {
		if cp.flipped {
			t.Fatalf("template %q marked flipped on a stats-free fixture", cp.key)
		}
	}
	_ = chickpeas.NodeID(0)
}
