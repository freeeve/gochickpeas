// Flip-aware cache routing: a template whose blind plan differs
// structurally from sighted planning executes through the sighted path
// (results were always identical -- the flip is a COST hazard -- so these
// tests pin the routing and the detection default, and the census
// measures the cost effect).
package gql

import (
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
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
	g := SocialGraph(t)
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
	g := SocialGraph(t)
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
	g := SocialGraph(t)
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

// TestCanonicalRenderStableAndTimeFree is the teeth check on the flip
// detector's own input (task 252): planFlipped compares two independent
// Canonical renders, so any volatile content -- a wall-clock line like
// Render's "Planning: ... ms", map-order nondeterminism -- would mark
// every template flipped and silently bypass the cache for everything
// (the inverse polarity of the always-passing comparison this ask
// reports). Two renders of one plan must be byte-identical, and no
// canonical line may carry a timing marker.
func TestCanonicalRenderStableAndTimeFree(t *testing.T) {
	g := SocialGraph(t)
	q, err := parseDesugar("MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 30 RETURN f.name AS n ORDER BY n")
	if err != nil {
		t.Fatal(err)
	}
	gr := graph.New(g)
	p, err := plan.Build(q, gr)
	if err != nil {
		t.Fatal(err)
	}
	r1 := explain.Canonical(p, plan.Estimate(p, gr))
	r2 := explain.Canonical(p, plan.Estimate(p, gr))
	if strings.Join(r1, "\n") != strings.Join(r2, "\n") {
		t.Fatalf("canonical render is nondeterministic:\n%v\nvs\n%v", r1, r2)
	}
	for _, ln := range r1 {
		if strings.Contains(ln, "Planning:") || strings.Contains(ln, " ms") {
			t.Fatalf("canonical render carries a timing line: %q", ln)
		}
	}
}

// TestFlippedTemplateSightedPlanCached pins the flipped route's plan
// reuse: the first L1 hit plans the literal text once and stores it on
// the entry; later hits execute it without re-planning, with rows
// identical to the uncached path.
func TestFlippedTemplateSightedPlanCached(t *testing.T) {
	g := SocialGraph(t)
	c := NewPlanCache(0)
	q := "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n ORDER BY n"
	want := runRows(t, func() (*Rows, error) { return Run(g, q) })
	_ = runRows(t, func() (*Rows, error) { return c.Run(g, q) }) // insert
	for _, cp := range c.byTemplate {
		cp.flipped = true
	}
	got := runRows(t, func() (*Rows, error) { return c.Run(g, q) }) // first flipped hit: builds sighted
	c.mu.Lock()
	e := c.byQuery[q]
	c.mu.Unlock()
	if e == nil || e.sighted == nil {
		t.Fatal("first flipped L1 hit did not cache the sighted plan")
	}
	sp := e.sighted.plan
	got2 := runRows(t, func() (*Rows, error) { return c.Run(g, q) }) // reuse
	c.mu.Lock()
	same := c.byQuery[q].sighted.plan == sp
	c.mu.Unlock()
	if !same {
		t.Fatal("second flipped hit re-planned instead of reusing the sighted plan")
	}
	for _, rows := range [][]string{got, got2} {
		if len(rows) != len(want) {
			t.Fatalf("flipped rows = %d, want %d", len(rows), len(want))
		}
		for i := range want {
			if rows[i] != want[i] {
				t.Fatalf("flipped row %d = %q, want %q", i, rows[i], want[i])
			}
		}
	}
	// The entry was charged for the second plan.
	if e.bytes <= l1Overhead+len(q) {
		t.Fatalf("entry bytes %d do not include the sighted charge", e.bytes)
	}
	// EXPLAIN through a flipped entry keeps the uncached route (no
	// sighted caching for render modes).
	qe := "EXPLAIN " + q
	_ = runRows(t, func() (*Rows, error) { return c.Run(g, qe) })
	for _, cp := range c.byTemplate {
		cp.flipped = true
	}
	_ = runRows(t, func() (*Rows, error) { return c.Run(g, qe) })
	c.mu.Lock()
	ee := c.byQuery[qe]
	c.mu.Unlock()
	if ee != nil && ee.sighted != nil {
		t.Fatal("EXPLAIN cached a sighted plan; render modes must stay uncached")
	}
}
