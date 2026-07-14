// Canonical plan-shape snapshots (task 112): the golden-plan primitive must
// render a query's plan to a stable string that pins shape, op order, scan
// sources, fired recognizers, and the chosen anchor -- while excluding the
// volatile parts (wall-clock planning time, the est header, per-operator
// estimates, and the anchor note's card/fan-out magnitudes) so a diff means a
// planner change moved a plan, not that an estimate drifted.
package explain

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

func canonicalOf(t *testing.T, g graph.Graph, src string) string {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return strings.Join(Canonical(p, plan.Estimate(p, g)), "\n")
}

// TestCanonicalExcludesVolatile: the canonical form must carry none of the
// run-to-run / fixture-drift noise -- no EXPLAIN/Planning header, no est column
// header, and every anchor-note magnitude normalized to N.
func TestCanonicalExcludesVolatile(t *testing.T) {
	g := fixture(t)
	c := canonicalOf(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) WHERE m.len > 20 RETURN m.len AS len ORDER BY len LIMIT 5")

	for _, bad := range []string{"EXPLAIN", "Planning:"} {
		if strings.Contains(c, bad) {
			t.Fatalf("canonical leaked volatile %q:\n%s", bad, c)
		}
	}
	for _, ln := range strings.Split(c, "\n") {
		if strings.TrimSpace(ln) == "est" || strings.HasSuffix(strings.TrimSpace(ln), " est") {
			t.Fatalf("canonical kept the est header line %q:\n%s", ln, c)
		}
	}
	// Anchor-note magnitudes are normalized: every "card=" is followed by N.
	if !strings.Contains(c, "card=N") {
		t.Fatalf("expected a normalized anchor note (card=N):\n%s", c)
	}
	for s := c; ; {
		i := strings.Index(s, "card=")
		if i < 0 {
			break
		}
		rest := s[i+len("card="):]
		if len(rest) == 0 || rest[0] != 'N' {
			t.Fatalf("card= estimate magnitude not normalized to N:\n%s", c)
		}
		s = rest
	}
}

// TestCanonicalPinsShapeAnchorRecognizers: the form must pin exactly what a
// plan-regression corpus needs -- scan source kind, op order, the chosen anchor
// and its tie-break reason, and which recognizers fired.
func TestCanonicalPinsShapeAnchorRecognizers(t *testing.T) {
	g := fixture(t)

	seek := canonicalOf(t, g, "MATCH (a:Person)-[e:KNOWS]->{1,3}(b:Person) WHERE id(a) = 3 AND all(i IN range(0, size(rels(e)) - 2) WHERE rels(e)[i].ts > rels(e)[i+1].ts) RETURN b.pid")
	wantContains(t, seek,
		"NodeBySeek (a = id 3)",
		"[anchor: a:Person",
		"smaller leaf cardinality",
		"VarExpand (a)-[:KNOWS*1..3]->(b) [mono ts desc]", // mono recognizer pinned
		"Project [b.pid]",
	)
	// Op order is part of the shape: anchor scan precedes the expand precedes project.
	if !(strings.Index(seek, "NodeBySeek") < strings.Index(seek, "VarExpand") &&
		strings.Index(seek, "VarExpand") < strings.Index(seek, "Project")) {
		t.Fatalf("op order not preserved:\n%s", seek)
	}

	agg := canonicalOf(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN tg.name AS name, count(m) AS n")
	wantContains(t, agg,
		"NodeScan (tg:Tag)",
		"Expand (tg)<-[:HAS_TAG]-(m)",
		"Aggregate (group=[name]; count(m))", // aggregate recognizer pinned
	)

	prop := canonicalOf(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) RETURN m.len")
	wantContains(t, prop, "NodeByProperty (tg:Tag {name = 'tagA'})") // scan source kind pinned
}

// TestCanonicalStableAcrossPlannings: Go randomizes map iteration every run, so
// a canonical string that varies across plannings would be useless as a golden.
// This is the plan-stability guarantee (task 090) applied to the golden form.
func TestCanonicalStableAcrossPlannings(t *testing.T) {
	g := fixture(t)
	const src = "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) WHERE m.len > 20 RETURN m.len AS len ORDER BY len LIMIT 5"
	first := canonicalOf(t, g, src)
	for range 50 {
		if got := canonicalOf(t, g, src); got != first {
			t.Fatalf("canonical nondeterministic across plannings:\n--- first ---\n%s\n--- got ---\n%s", first, got)
		}
	}
}
