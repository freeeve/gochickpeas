// Shape tests for the early shortest-path row gate (gate.go): the gate
// injects exactly when the downstream chain is a pure per-row SP +
// LET/FILTER pipeline whose references the host can evaluate, and stays
// out for every disqualifying shape. Result parity is pinned end-to-end
// by the gql execution tests and the LDBC verify gate (Q10's pinned row
// hash exercises the fired gate).
package plan

import (
	"slices"
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

// TestStageSlotBinds pins the per-stage-kind slot-binding enumeration that
// batch-barrier placement relies on: every stage kind must report exactly the
// slots it binds (a missed slot would let a barrier split a producer from its
// consumer).
func TestStageSlotBinds(t *testing.T) {
	collect := func(st Stage) []int {
		var got []int
		stageSlotBinds(st, func(s int) { got = append(got, s) })
		slices.Sort(got)
		return got
	}
	cases := []struct {
		name string
		st   Stage
		want []int
	}{
		{"match", &MatchStage{
			Ops: []BindOp{
				{Kind: OpScan, Slot: 1},
				{Kind: OpExpand, To: 2, RelSlot: 3},
				{Kind: OpVarExpand, To: 4, RelSlot: 5},
			},
			PathBind: &PathBindSpec{PathSlot: 6},
		}, []int{1, 2, 3, 4, 5, 6}},
		{"hashjoin", &HashJoinStage{PayloadSlots: []int{7, 8}, KeySlot: 9}, []int{7, 8, 9}},
		{"sp", &SpStage{PathSlot: 10}, []int{10}},
		{"gate", &GateStage{Sp: SpStage{PathSlot: 11}, Derived: []GateDerived{{Slot: 12}, {Slot: 13}}}, []int{11, 12, 13}},
		{"call", &CallStage{NodeSlot: 14, ValueSlot: 15, DepthSlot: 16}, []int{14, 15, 16}},
		{"unwind", &UnwindStage{OutSlot: 17}, []int{17}},
		{"callsub", &CallSubqueryStage{OutSlots: []int{18, 19}}, []int{18, 19}},
	}
	for _, c := range cases {
		if got := collect(c.st); !slices.Equal(got, c.want) {
			t.Fatalf("%s slot binds = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStageUniqScopes pins the relationship-uniqueness scopes each stage
// participates in, the input to splitsUniqScope's barrier check: a MatchStage
// reports its own scope, and a HashJoinStage reports every build scope plus
// the probe's uniqueness scope when present.
func TestStageUniqScopes(t *testing.T) {
	collect := func(st Stage) []uint32 {
		var got []uint32
		stageUniqScopes(st, func(sc uint32) { got = append(got, sc) })
		slices.Sort(got)
		return got
	}
	if got := collect(&MatchStage{Scope: 100}); !slices.Equal(got, []uint32{100}) {
		t.Fatalf("match scope = %v, want [100]", got)
	}
	hj := &HashJoinStage{Build: []*MatchStage{{Scope: 200}}, Probe: BindOp{Uniq: &RelUniq{Scope: 201}}}
	if got := collect(hj); !slices.Equal(got, []uint32{200, 201}) {
		t.Fatalf("hashjoin scopes = %v, want [200 201]", got)
	}
	// With no probe uniqueness, only the build scopes report.
	hj2 := &HashJoinStage{Build: []*MatchStage{{Scope: 200}}}
	if got := collect(hj2); !slices.Equal(got, []uint32{200}) {
		t.Fatalf("hashjoin without probe uniq = %v, want [200]", got)
	}
	// A stage kind that participates in no uniqueness scope reports nothing.
	if got := collect(&SpStage{PathSlot: 1}); got != nil {
		t.Fatalf("SpStage scopes = %v, want none", got)
	}
}
