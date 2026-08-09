package exec

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestFilterBoundarySegment drives a standalone FILTER over a computed alias:
// RETURN a.v + 1 AS w NEXT FILTER w > 15 runs the FILTER as its own segment
// through the passthrough sink, keeping only the rows whose computed value
// passes the guard.
func TestFilterBoundarySegment(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for _, v := range []int64{10, 20, 30} {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "v", v)
	}
	g := graph.New(bld.Finalize("v"))
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH (a:A) RETURN a.v + 1 AS w NEXT FILTER w > 15 RETURN w")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}

	// v+1 gives {11, 21, 31}; FILTER w > 15 keeps {21, 31}.
	got := map[int64]bool{}
	for _, r := range rows {
		w, _ := r[0].AsInt()
		got[w] = true
	}
	if len(got) != 2 || !got[21] || !got[31] {
		t.Fatalf("post-WHERE results = %v, want {21, 31}", got)
	}
}

// TestAggregateSegmentPostWhere drives applyPostWhere: a FILTER fused onto
// an aggregating boundary filters the grouped output rows in place,
// including the collect-broadcast IN probe over a constant output column.
func TestAggregateSegmentPostWhere(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for _, v := range []int64{1, 1, 2, 3} {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "v", v)
	}
	g := graph.New(bld.Finalize("v"))
	ctx := &eval.Ctx{G: g}
	run := func(src string) [][]value.Value {
		t.Helper()
		q, err := parser.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatalf("plan %q: %v", src, err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatalf("exec %q: %v", src, err)
		}
		return rows
	}

	// Groups: v=1 (n=2), v=2 (n=1), v=3 (n=1); the boundary FILTER keeps
	// only the doubled group.
	rows := run("MATCH (a:A) RETURN a.v AS v, count(*) AS n NEXT FILTER n > 1 RETURN v, n")
	if len(rows) != 1 {
		t.Fatalf("filtered groups = %d, want 1", len(rows))
	}
	if v, _ := rows[0][0].AsInt(); v != 1 {
		t.Fatalf("kept group v = %d, want 1", v)
	}

	// IN over a collected broadcast column: every row carries the same
	// list, the membership probe keeps matching groups.
	rows = run("MATCH (a:A) RETURN a.v AS v, collect(a.v) AS all NEXT FILTER v IN all RETURN v")
	if len(rows) != 3 {
		t.Fatalf("membership-filtered groups = %d, want 3", len(rows))
	}
}

// TestIdentityPassthroughMatchesGeneral is the differential for the
// pass-through boundary fast path: each fixture runs with the identity
// path and pinned to the general copy-through pipeline
// (disableIdentPassthrough), and the ordered row lists must match.
//
// Injection-audit protocol (task 276's two degeneracy modes): re-apply a
// drop-every-third-row injection inside the fast path and check EVERY
// engaged fixture diverges -- a fixture can guard nothing either because
// its retained set is stable under the fault (fix: widen the bound) or
// because its keys share a period with the injection stride (fix: change
// the key or the stride). Expected under that injection: all fixtures
// with engage=true diverge. Re-run the audit when adding a fixture.
func TestIdentityPassthroughMatchesGeneral(t *testing.T) {
	g := colAggFixture(t)
	// A streamable boundary fuses into its producer's segment run, so a
	// lone no-stage segment -- the fast path's habitat -- arises when a
	// NON-streamable boundary follows another non-streamable one. The
	// aggregate-then-ORDER BY shapes below are exactly BI/Q4's ordering
	// stage; the trailing identity RETURN of a query is the other
	// single-segment run and also engages.
	queries := []struct {
		q      string
		engage bool
	}{
		// Identity re-emit with ORDER BY after an aggregated boundary
		// (the Q4-class ordering stage).
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN l, n ORDER BY n DESC, l NEXT RETURN l, n ORDER BY l LIMIT 500`, true},
		// Standalone ORDER BY statement (star boundary) + pagination.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT ORDER BY n, l SKIP 3 LIMIT 7 RETURN l, n ORDER BY l`, true},
		// Rename-only boundary: values are the input rows verbatim.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN l AS a, n AS c ORDER BY c DESC, a NEXT RETURN a, c ORDER BY a`, true},
		// ORDER BY key expression over a column (not a bare column read).
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN l, n ORDER BY l % 7, n, l NEXT RETURN l, n ORDER BY l`, true},
		// All-streamable query: every boundary fuses into one segment run,
		// so no lone no-stage segment exists and the fast path never runs
		// (streaming already avoids the materialization it would skip).
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f NEXT RETURN l, f`, false},
		// Reordered columns are NOT identity: general path, still correct.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN n, l ORDER BY l`, false},
		// Computed column declines.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN n + 1 AS n1 ORDER BY n1`, false},
		// DISTINCT declines.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN DISTINCT l ORDER BY l`, false},
	}
	disableColAgg = true
	defer func() { disableColAgg = false }()
	for i, tc := range queries {
		before := identPassthroughs
		fast := runOrdered(t, g, tc.q)
		fired := identPassthroughs != before
		disableIdentPassthrough = true
		baseline := identPassthroughs
		general := runOrdered(t, g, tc.q)
		disableIdentPassthrough = false
		if identPassthroughs != baseline {
			t.Fatalf("query %d: disabled path still passed through (switch dead)", i)
		}
		if fmt.Sprint(fast) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfast:    %v\ngeneral: %v", i, fast, general)
		}
		if fired != tc.engage {
			t.Errorf("query %d: passthrough engagement = %v, want %v (vacuity guard)", i, fired, tc.engage)
		}
	}
}
