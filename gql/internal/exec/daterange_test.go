package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// dayMS is 2012-09-16T00:00:00Z in epoch millis (15599 days).
const dayMS = int64(15599) * eval.MSPerDay

// dateRangeFixture builds nodes whose ts values straddle every boundary
// of the target day: exact midnight, last millisecond, next midnight,
// previous millisecond, a mid-day value, a pre-1970 negative epoch, and
// one node with no ts at all.
func dateRangeFixture(t *testing.T) graph.Graph {
	t.Helper()
	bld := chickpeas.NewBuilder(16, 0)
	vals := []int64{
		dayMS,                     // midnight, in
		dayMS + eval.MSPerDay - 1, // last ms, in
		dayMS + eval.MSPerDay,     // next midnight, out
		dayMS - 1,                 // previous day, out
		dayMS + 12*3_600_000,      // mid-day, in
		-5 * eval.MSPerDay,        // pre-1970, out
	}
	for i, v := range vals {
		if _, err := bld.AddNode("X"); err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(chickpeas.NodeID(i), "ts", v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bld.AddNode("X"); err != nil { // no ts
		t.Fatal(err)
	}
	return graph.New(bld.Finalize())
}

// runQuery plans and executes q, returning the row count.
func runQuery(t *testing.T, g graph.Graph, q string) int {
	t.Helper()
	ast, err := parser.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ast, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(&eval.Ctx{G: g}, p)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// TestDateEqRangeRewrite pins the truncation-equality rewrite (task 205
// round 4): date(n.ts) = <const Date> over an integer column must rewrite
// to the half-open epoch range (engagement counter climbs), keep exactly
// the in-day rows across every boundary, and agree exactly with the
// un-rewritten general evaluation.
func TestDateEqRangeRewrite(t *testing.T) {
	g := dateRangeFixture(t)
	q := "MATCH (n:X) WHERE date(n.ts) = date(zoned_datetime('2012-09-16')) RETURN n"

	before := dateRangeRewrites
	got := runQuery(t, g, q)
	if dateRangeRewrites == before {
		t.Fatal("rewrite did not fire on the qualifying shape")
	}
	if got != 3 {
		t.Fatalf("rewritten form kept %d rows, want 3 (midnight, last-ms, mid-day)", got)
	}

	disableDateRangeRewrite = true
	defer func() { disableDateRangeRewrite = false }()
	if general := runQuery(t, g, q); general != got {
		t.Fatalf("general evaluation kept %d rows, rewrite kept %d -- the rewrite changes results", general, got)
	}
}

// TestDateEqRangeRewriteRefusals pins the fail-closed gates end-to-end:
// a row-dependent comparison side, a non-property date() argument, and a
// string-typed column must all keep the general evaluation (the counter
// stays flat) while still executing correctly.
func TestDateEqRangeRewriteRefusals(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for i := range 2 {
		if _, err := bld.AddNode("S"); err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(chickpeas.NodeID(i), "sd", "2012-09-16"); err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(chickpeas.NodeID(i), "ts", dayMS+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize())

	cases := []struct {
		name, q string
		rows    int
	}{
		// sd is a string column: date('2012-09-16') parses, so the rows
		// match -- but only through the general evaluation.
		{"string column", "MATCH (n:S) WHERE date(n.sd) = date(zoned_datetime('2012-09-16')) RETURN n", 2},
		// The comparison side reads another row variable.
		{"row-dependent side", "MATCH (n:S), (m:S) WHERE date(n.ts) = date(m.ts) RETURN n, m", 4},
		// The date() argument is not a bare property read.
		{"non-prop argument", "MATCH (n:S) WHERE date(n.ts + 0) = date(zoned_datetime('2012-09-16')) RETURN n", 2},
	}
	for _, tc := range cases {
		before := dateRangeRewrites
		if got := runQuery(t, g, tc.q); got != tc.rows {
			t.Fatalf("%s: %d rows, want %d", tc.name, got, tc.rows)
		}
		if dateRangeRewrites != before {
			t.Fatalf("%s: the rewrite fired on a shape its gates must refuse", tc.name)
		}
	}

	// Value-kind gate control: the same ts shape over this graph's i64
	// column must fire (the refusals above are gate-specific, not a dead
	// mechanism).
	before := dateRangeRewrites
	if got := runQuery(t, g, "MATCH (n:S) WHERE date(n.ts) = date(zoned_datetime('2012-09-16')) RETURN n"); got != 2 {
		t.Fatalf("control: %d rows, want 2", got)
	}
	if dateRangeRewrites == before {
		t.Fatal("control shape did not fire -- the refusal cases are vacuous")
	}
}
