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

// pctConstEval is a RowEval that ignores its row and returns a fixed value:
// the percentile argument fed to percentileOf in these tests.
type pctConstEval struct{ v value.Value }

func (c pctConstEval) Eval(*eval.Ctx, []value.Value, map[string]int) value.Value { return c.v }

// TestPercentileOfGuards covers the Null-returning guards: no expression,
// an empty group, a percentile outside [0,1], and a non-numeric percentile.
func TestPercentileOfGuards(t *testing.T) {
	ctx := &eval.Ctx{}
	vals := []value.Value{value.Int(10), value.Int(20)}
	if got := percentileOf(ctx, nil, vals, false); !got.IsNull() {
		t.Fatalf("nil pc = %v, want null", got)
	}
	if got := percentileOf(ctx, pctConstEval{value.Float(0.5)}, nil, false); !got.IsNull() {
		t.Fatalf("empty group = %v, want null", got)
	}
	for _, p := range []float64{-0.1, 1.5} {
		if got := percentileOf(ctx, pctConstEval{value.Float(p)}, []value.Value{value.Int(1)}, true); !got.IsNull() {
			t.Fatalf("p=%v = %v, want null", p, got)
		}
	}
	if got := percentileOf(ctx, pctConstEval{value.Str("half")}, []value.Value{value.Int(1)}, true); !got.IsNull() {
		t.Fatalf("non-float p = %v, want null", got)
	}
}

// unsorted returns a fresh out-of-order group; percentileOf sorts in place,
// so each call gets its own copy.
func unsorted() []value.Value {
	return []value.Value{value.Int(30), value.Int(10), value.Int(40), value.Int(20)}
}

// TestPercentileDisc covers PERCENTILE_DISC nearest-rank selection
// (ceil(p*n) clamped to [1,n], 1-based) returning the collected value with
// its kind unchanged, and that the group is sorted first.
func TestPercentileDisc(t *testing.T) {
	ctx := &eval.Ctx{}
	for _, c := range []struct {
		p    float64
		want int64
	}{{0.0, 10}, {0.25, 10}, {0.5, 20}, {0.75, 30}, {1.0, 40}} {
		got := percentileOf(ctx, pctConstEval{value.Float(c.p)}, unsorted(), false)
		if got.Kind() != value.KindInt {
			t.Fatalf("disc p=%v kind = %v, want Int (unchanged)", c.p, got.Kind())
		}
		if iv, _ := got.AsInt(); iv != c.want {
			t.Fatalf("disc p=%v = %v, want %d", c.p, got, c.want)
		}
	}
}

// TestPercentileCont covers PERCENTILE_CONT linear interpolation between the
// two straddling values over the sorted group, always returning Float.
func TestPercentileCont(t *testing.T) {
	ctx := &eval.Ctx{}
	// Sorted group is [10,20,30,40], n-1 = 3; rank = p*3.
	for _, c := range []struct {
		p    float64
		want float64
	}{{0.0, 10}, {0.25, 17.5}, {0.5, 25}, {1.0, 40}} {
		got := percentileOf(ctx, pctConstEval{value.Float(c.p)}, unsorted(), true)
		if got.Kind() != value.KindFloat {
			t.Fatalf("cont p=%v kind = %v, want Float", c.p, got.Kind())
		}
		if fv, _ := got.AsFloat(); fv != c.want {
			t.Fatalf("cont p=%v = %v, want %v", c.p, got, c.want)
		}
	}
}

// runOrdered executes q and renders its rows IN ORDER (no sort: ORDER BY
// output order is the subject under test).
func runOrdered(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(qq, graph.New(g))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := Execute(&eval.Ctx{G: graph.New(g)}, p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r))
	}
	return out
}

// TestAggTopKMatchesSort is the differential for the aggregated bounded
// ORDER BY + LIMIT path: every shape runs streamed (finalizeTopK) and
// materialized (disableAggTopk), and the ordered row lists must be
// identical -- group-key and aggregate-output keys, ascending/descending,
// a non-column key expression, post-aggregation wrapper columns, key ties
// straddling the LIMIT boundary (group arrival order decides, matching
// the sort's index tiebreak), SKIP consuming into the bound, LIMIT 0,
// a bound past the group count, arg-level DISTINCT, and a keyless
// aggregate. colagg is disabled throughout so the general aggregator --
// the subject -- executes on both sides.
func TestAggTopKMatchesSort(t *testing.T) {
	g := colAggFixture(t)
	queries := []string{
		// Aggregate-output key, descending, bound < groups.
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY n DESC, l ASC LIMIT 5`,
		// Group-key ORDER BY ascending.
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY l LIMIT 7`,
		// Non-column key expression (evaluates over the finalized row).
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY l % 7, n DESC, l LIMIT 6`,
		// Post-aggregation wrapper column as the key. LIMIT 9 rather than
		// smaller: the drop-every-third-group injection audit (task 275's
		// lesson -- verify each fixture against the injection itself)
		// showed a tighter bound's top rows can all survive the dropped
		// groups, leaving this fixture unable to catch a broken stream.
		`MATCH (m:Message) RETURN m.length AS l, sum(m.length) / CAST(count(m) AS FLOAT) AS avgLen ORDER BY avgLen DESC, l LIMIT 9`,
		// Key ties straddling the boundary: flag has 3 values (true/false/
		// null) but the CASE key collapses to 2, forcing equal keys at the
		// cut; arrival order must decide identically both ways.
		`MATCH (m:Message) RETURN CASE WHEN m.length < 100 THEN 0 ELSE 1 END AS b, m.length AS l, count(m) AS n ORDER BY b LIMIT 3`,
		// SKIP consumes into the bound.
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY l DESC SKIP 3 LIMIT 4`,
		// LIMIT 0 and bound past the group count.
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY l LIMIT 0`,
		`MATCH (m:Message) RETURN m.length AS l, count(m) AS n ORDER BY l LIMIT 1000`,
		// Arg-level DISTINCT stays eligible (projection-level does not).
		`MATCH (m:Message) RETURN m.flag AS f, count(DISTINCT m.length) AS d ORDER BY d DESC, f LIMIT 2`,
		// Keyless aggregate: one row either way.
		`MATCH (m:Message) RETURN count(m) AS n, min(m.length) AS lo ORDER BY n LIMIT 1`,
	}
	disableColAgg = true
	defer func() { disableColAgg = false }()
	for i, q := range queries {
		before := aggTopkBuilds
		streamed := runOrdered(t, g, q)
		fired := aggTopkBuilds != before
		disableAggTopk = true
		baseline := aggTopkBuilds
		sorted := runOrdered(t, g, q)
		disableAggTopk = false
		if aggTopkBuilds != baseline {
			t.Fatalf("query %d: disabled path still built top-k rows (switch dead)", i)
		}
		if fmt.Sprint(streamed) != fmt.Sprint(sorted) {
			t.Errorf("query %d diverged:\nstreamed: %v\nsorted:   %v", i, streamed, sorted)
		}
		// LIMIT 0 admits nothing (the counter counts builds); a bound at or
		// past the group count takes the sort path by the nGroups > bound
		// gate (LIMIT 1000, and the keyless aggregate where nGroups ==
		// bound == 1). Every other shape must engage the streamed path.
		expectFired := i != 6 && i != 7 && i != 9
		if fired != expectFired {
			t.Errorf("query %d: top-k engagement = %v, want %v (vacuity guard)\nplan:\n%s",
				i, fired, expectFired, planShape(t, g, q))
		}
	}
}
