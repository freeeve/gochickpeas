package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestGroupJoinExecMatchesNested drives the OPTIONAL-MATCH group-join
// decorrelation sink and pins the fast path to the general path: the
// decorrelated aggregate (forced via the outer-rows floor knob) must produce
// exactly the same per-anchor counts as the nested OPTIONAL execution.
// Fixture: p0-KNOWS->p1, p0-KNOWS->p2, p1-KNOWS->p2; p3 has no KNOWS.
func TestGroupJoinExecMatchesNested(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("Person"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		if _, err := bld.AddRel(graph.NodeID(e[0]), graph.NodeID(e[1]), "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, count(b) AS c")
	if err != nil {
		t.Fatal(err)
	}
	counts := func() map[graph.NodeID]int64 {
		t.Helper()
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		out := map[graph.NodeID]int64{}
		for _, r := range rows {
			id, _ := r[0].AsNode()
			c, _ := r[1].AsInt()
			out[id] = c
		}
		return out
	}

	defer func(v float64) { plan.GroupJoinMinOuterRows = v }(plan.GroupJoinMinOuterRows)

	// The nested OPTIONAL execution (the group-join rewrite disabled).
	plan.GroupJoinMinOuterRows = 1e18
	nested := counts()
	// The decorrelated group-join (rewrite forced onto the small fixture).
	plan.GroupJoinMinOuterRows = 0
	gj := counts()

	// The two paths must agree exactly.
	if len(gj) != len(nested) {
		t.Fatalf("group-join result size %d != nested %d", len(gj), len(nested))
	}
	for id, c := range nested {
		if gj[id] != c {
			t.Fatalf("group-join count[%d] = %d, nested = %d", id, gj[id], c)
		}
	}
	// And the counts are the expected KNOWS out-degrees (0 for null-extended).
	for id, want := range map[graph.NodeID]int64{0: 2, 1: 1, 2: 0, 3: 0} {
		if nested[id] != want {
			t.Fatalf("count[%d] = %d, want %d", id, nested[id], want)
		}
	}

	// Control (ragedb's 295 lesson): the PLAIN match variant must never
	// gain zero-count rows -- an anchor-derived count that truncates the
	// pattern would emit one row per Person, reporting 0 for the edgeless
	// -- and the rewrite knob being forced must not change that, since the
	// detector admits OPTIONAL only. Anchors without a KNOWS edge produce
	// NO row here, not a zero.
	plain, err := parser.Parse("MATCH (a:Person)-[:KNOWS]->(b) RETURN a, count(b) AS c")
	if err != nil {
		t.Fatal(err)
	}
	q = plain
	for _, floor := range []float64{1e18, 0} {
		plan.GroupJoinMinOuterRows = floor
		got := counts()
		if len(got) != 2 {
			t.Fatalf("plain match (floor=%g): %d rows, want 2 (edgeless anchors must not appear)", floor, len(got))
		}
		if got[0] != 2 || got[1] != 1 {
			t.Fatalf("plain match (floor=%g): counts = %v, want {0:2, 1:1}", floor, got)
		}
		if _, invented := got[2]; invented {
			t.Fatalf("plain match (floor=%g): invented a zero-count row for an edgeless anchor", floor)
		}
	}
}

// TestGroupJoinDeclinesSharedUniqScope pins the detector's uniqueness-scope
// guard: comma patterns in one OPTIONAL MATCH share a relationship-
// uniqueness scope, so the nested walk of the last pattern excludes rels the
// sibling pattern used -- an exclusion the decorrelated standalone inner
// cannot see. On a single undirected KNOWS edge, the nested count is 0 for
// both anchors (the sibling consumed the only rel); a decorrelated inner
// would count 1. The detector must decline the shape entirely.
func TestGroupJoinDeclinesSharedUniqScope(t *testing.T) {
	bld := chickpeas.NewBuilder(4, 4)
	for range 2 {
		if _, err := bld.AddNode("Person"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bld.AddRel(0, 1, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:Person) OPTIONAL MATCH (a)-[r:KNOWS]-(m), (m)-[s:KNOWS]-(k) RETURN a, count(k) AS c")
	if err != nil {
		t.Fatal(err)
	}
	defer func(v float64) { plan.GroupJoinMinOuterRows = v }(plan.GroupJoinMinOuterRows)
	counts := func() map[graph.NodeID]int64 {
		t.Helper()
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		out := map[graph.NodeID]int64{}
		for _, r := range rows {
			id, _ := r[0].AsNode()
			c, _ := r[1].AsInt()
			out[id] = c
		}
		return out
	}
	plan.GroupJoinMinOuterRows = 1e18
	nested := counts()
	for id, want := range map[graph.NodeID]int64{0: 0, 1: 0} {
		if nested[id] != want {
			t.Fatalf("nested count[%d] = %d, want %d (scope must exclude the sibling's rel)", id, nested[id], want)
		}
	}
	plan.GroupJoinMinOuterRows = 0
	forced := counts()
	for id, want := range nested {
		if forced[id] != want {
			t.Fatalf("forced-floor count[%d] = %d, nested = %d: the rewrite fired on a shared-scope comma pattern", id, forced[id], want)
		}
	}
	// The agreement must come from the guard, not from luck in the inner:
	// the forced-floor plan must contain no GroupJoinStage.
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range p.Branches {
		for _, seg := range br {
			for _, st := range seg.Stages {
				if _, isGJ := st.(*plan.GroupJoinStage); isGJ {
					t.Fatal("detector admitted a comma pattern sharing a uniqueness scope")
				}
			}
		}
	}
}
