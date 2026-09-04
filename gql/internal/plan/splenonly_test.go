// Marking matrix for the shortest-path materialization elision: a single
// unweighted ANY SHORTEST whose path is read only as length(p) marks
// LengthOnly and rewrites the reads; any other path use, the ALL form,
// and the weighted form refuse -- pinned at the plan so the exec
// differential cannot pass vacuously.
package plan

import (
	"testing"
)

func spLenDelta(t *testing.T, src string) int {
	t.Helper()
	g := buildFixture(t)
	before := spLenElides
	mustPlan(t, g, src)
	return spLenElides - before
}

func TestSpLenOnlyFires(t *testing.T) {
	for _, src := range []string{
		// The Q10 shape: SP, LET on length, FILTER, aggregate.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 MATCH (e)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 LET dist = length(p) FILTER dist >= 2
		 RETURN e.pid AS pid, count(m) AS c`,
		// Direct filter on length(p), no LET.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 RETURN s, e
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 FILTER length(p) >= 2
		 RETURN e.pid AS pid`,
		// Unbounded quantifier: {1,} has no Max and must still fire
		// (the rustychickpeas corpus IC13 shape; their 365).
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 RETURN s, e
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,}(e)
		 RETURN e.pid AS pid, length(p) AS dist ORDER BY pid`,
		// length(p) as an output expression.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 RETURN s, e
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 RETURN e.pid AS pid, length(p) AS dist ORDER BY pid`,
	} {
		if d := spLenDelta(t, src); d != 1 {
			t.Fatalf("spLenElides moved %d on %q, want 1", d, src)
		}
	}
}

func TestSpLenOnlyDeclines(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"rels-use",
			`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
			 RETURN s, e
			 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
			 RETURN e.pid AS pid, size(rels(p)) AS n ORDER BY pid`},
		{"nodes-use",
			`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
			 RETURN s, e
			 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
			 RETURN e.pid AS pid, size(nodes(p)) AS n ORDER BY pid`},
		{"bare-projection",
			`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
			 RETURN s, e
			 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
			 RETURN p AS p`},
		{"all-form",
			`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
			 RETURN s, e
			 NEXT MATCH p = ALL SHORTEST (s)-[:KNOWS]-{1,4}(e)
			 RETURN e.pid AS pid, length(p) AS dist ORDER BY pid, dist`},
		{"mixed-len-and-rels",
			`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
			 RETURN s, e
			 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
			 LET dist = length(p)
			 RETURN e.pid AS pid, dist AS dist, size(rels(p)) AS n`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := spLenDelta(t, tc.src); d != 0 {
				t.Fatalf("spLenElides moved %d, want 0", d)
			}
		})
	}
}

func TestSpLenOnlyDisableSwitch(t *testing.T) {
	DisableSpLenOnly = true
	defer func() { DisableSpLenOnly = false }()
	src := `MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
	 RETURN s, e
	 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
	 RETURN e.pid AS pid, length(p) AS dist ORDER BY pid`
	if d := spLenDelta(t, src); d != 0 {
		t.Fatalf("spLenElides moved %d with the pass disabled, want 0", d)
	}
}
