// M20 PROFILE tests: the annotated plan reports each operator's actual
// produced-row count -- the Rust profile_reports_actual_cardinalities and
// pushdown-pruning assertions translated to GQL.
package gql

import (
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// planText joins a PROFILE/EXPLAIN result's plan column.
func planText(t *testing.T, g *chickpeas.Snapshot, q string) string {
	t.Helper()
	rows, err := Run(g, q)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", q, err)
	}
	var lines []string
	for r := range rows.All() {
		v, _ := r.GetAt(0)
		s, _ := v.AsStr()
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n")
}

// lineWith returns the first plan line containing every needle.
func lineWith(p string, needles ...string) string {
	for _, l := range strings.Split(p, "\n") {
		ok := true
		for _, n := range needles {
			if !strings.Contains(l, n) {
				ok = false
				break
			}
		}
		if ok {
			return l
		}
	}
	return ""
}

func TestProfileReportsActualCardinalities(t *testing.T) {
	g := socialGraph(t)
	// 4 people scanned, 2 pass age > 30, 1 aggregate row.
	p := planText(t, g,
		"PROFILE MATCH (p:Person) WHERE p.age > 30 RETURN count(DISTINCT p.name) AS c")
	if !strings.HasPrefix(p, "PROFILE") {
		t.Fatalf("missing PROFILE header:\n%s", p)
	}
	if l := lineWith(p, "NodeScan", "4"); l == "" {
		t.Fatalf("no NodeScan line with 4:\n%s", p)
	}
	if l := lineWith(p, "Filter", "2"); l == "" {
		t.Fatalf("no Filter line with 2:\n%s", p)
	}
	if l := lineWith(p, "Aggregate", "1"); l == "" {
		t.Fatalf("no Aggregate line with 1:\n%s", p)
	}
}

func TestProfilePushdownPrunesBeforeExpand(t *testing.T) {
	g := socialGraph(t)
	// a.age > 30 references only the anchor, so it pushes to op 0 and
	// prunes there: only Bob and Carol expand, binding their 4
	// out-neighbors at the Expand. Without pushdown all four people would
	// expand (7 bindings) before the filter.
	p := planText(t, g,
		"PROFILE MATCH (a:Person)-[:KNOWS]->(b:Person) WHERE a.age > 30 RETURN count(DISTINCT b.name) AS c")
	expand := lineWith(p, "Expand")
	if expand == "" {
		t.Fatalf("no Expand line:\n%s", p)
	}
	// The rightmost column is the actual rows count (the estimate column
	// renders beside it and legitimately reads 7 -- the unpruned fan-out).
	fields := strings.Fields(expand)
	if got := fields[len(fields)-1]; got != "4" {
		t.Fatalf("expand should bind 4 (pruned), got %s: %s\nfull:\n%s", got, expand, p)
	}
}

func TestProfileBoundaryAndStageCounts(t *testing.T) {
	g := socialGraph(t)
	// The boundary FILTER count shows the surviving projected rows, and a
	// FOR stage records its expanded row count.
	p := planText(t, g,
		"PROFILE MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS name, count(*) AS n NEXT FILTER n > 1 RETURN name")
	// Segment 1 aggregates to 2 groups; the boundary filter keeps 1.
	if l := lineWith(p, "Aggregate", "2"); l == "" {
		t.Fatalf("no Aggregate line with 2 groups:\n%s", p)
	}
	if l := lineWith(p, "Filter (", "1"); l == "" {
		t.Fatalf("no boundary Filter line with 1 survivor:\n%s", p)
	}
	p = planText(t, g, "PROFILE FOR x IN [1, 2, 3] RETURN x AS x")
	if l := lineWith(p, "Unwind", "3"); l == "" {
		t.Fatalf("no Unwind line with 3 rows:\n%s", p)
	}
}

func TestProfileMultiBranchOrder(t *testing.T) {
	g := socialGraph(t)
	// Branch-major, segment-minor: each branch's scan shows its own count.
	p := planText(t, g,
		"PROFILE MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (c:Company) RETURN c.name AS n")
	person := lineWith(p, "NodeScan", ":Person", "4")
	company := lineWith(p, "NodeScan", ":Company", "2")
	if person == "" || company == "" {
		t.Fatalf("per-branch scan counts missing:\n%s", p)
	}
	if !strings.Contains(p, "UNION ALL") || !strings.Contains(p, "Branch 1") {
		t.Fatalf("branch structure missing:\n%s", p)
	}
}

func TestProfileOptionalAndShortest(t *testing.T) {
	g := socialGraph(t)
	// A shortest-path stage records one produced row.
	p := planText(t, g,
		"PROFILE MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH q = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(q) AS l")
	if l := lineWith(p, "ShortestPath", "1"); l == "" {
		t.Fatalf("no ShortestPath line with 1 row:\n%s", p)
	}
}
