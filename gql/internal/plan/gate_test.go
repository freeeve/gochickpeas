// Shape tests for the early shortest-path row gate (gate.go): the gate
// injects exactly when the downstream chain is a pure per-row SP +
// LET/FILTER pipeline whose references the host can evaluate, and stays
// out for every disqualifying shape. Result parity is pinned end-to-end
// by the gql execution tests and the LDBC verify gate (Q10's pinned row
// hash exercises the fired gate).
package plan

import (
	"testing"
)

// gateCount counts injected gates across a plan's segments.
func gateCount(p *Plan) int {
	n := 0
	for _, br := range p.Branches {
		for _, seg := range br {
			for _, st := range seg.Stages {
				if _, ok := st.(*GateStage); ok {
					n++
				}
			}
		}
	}
	return n
}

func TestSPGateInjection(t *testing.T) {
	g := buildFixture(t)
	fires := []string{
		// The Q10 shape: expansion segment, then SP, LET, FILTER.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 MATCH (e)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 LET dist = length(p) FILTER dist >= 2
		 RETURN e.pid AS pid, count(m) AS c`,
		// Filter directly on the path, no LET.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 MATCH (e)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 FILTER length(p) >= 2
		 RETURN e.pid AS pid, count(m) AS c`,
	}
	for _, src := range fires {
		if got := gateCount(mustPlan(t, g, src)); got != 1 {
			t.Errorf("expected 1 gate, got %d for:\n%s", got, src)
		}
	}

	skips := []string{
		// ALL SHORTEST expands rows: not a filter.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m
		 NEXT MATCH p = ALL SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 FILTER length(p) >= 2
		 RETURN e.pid AS pid, count(m) AS c`,
		// No filter behind the SP: nothing to gate on.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 RETURN e.pid AS pid, count(m) AS c`,
		// Aggregated host boundary: filtering its input does not commute.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, count(m) AS c
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 FILTER length(p) >= 2
		 RETURN e.pid AS pid, c`,
		// One filter conjunct references a computed column the host cannot
		// evaluate; the length(p) conjunct alone still gates.
		`MATCH (s:Person {pid: 0}) MATCH (e:Person)-[:KNOWS]-(:Person {pid: 3})
		 MATCH (e)<-[:HAS_CREATOR]-(m:Message)
		 RETURN s, e, m.len + 1 AS bump
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 FILTER length(p) >= 2 AND bump > 0
		 RETURN e.pid AS pid, count(bump) AS c`,
	}
	for i, src := range skips[:3] {
		if got := gateCount(mustPlan(t, g, src)); got != 0 {
			t.Errorf("skip case %d: expected 0 gates, got %d", i, got)
		}
	}
	// The mixed case still gates on the evaluable conjunct alone.
	p := mustPlan(t, g, skips[3])
	if got := gateCount(p); got != 1 {
		t.Errorf("mixed deps: expected 1 gate (length(p) conjunct only), got %d", got)
	}
}
