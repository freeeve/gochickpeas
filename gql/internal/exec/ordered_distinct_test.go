package exec

import (
	"fmt"
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// TestArgminTopKEvictionAndReentry pins the bounded accumulator's safety
// argument directly: an evicted candidate's stale minimum is irrelevant,
// and a later better row of the same tuple re-enters on its own key. One
// asc key, bound 2, single int column doubling as tuple identity.
func TestArgminTopKEvictionAndReentry(t *testing.T) {
	offer := func(a *argminTopK, id, key, seq int) {
		row := []value.Value{value.Int(int64(id))}
		a.offer(row, []int{0}, []value.Value{value.Int(int64(key))}, seq)
	}
	render := func(a *argminTopK) string { return fmt.Sprint(a.sorted()) }

	// Tuple 1 enters (key 10), tuple 2 enters (key 20), tuple 3 beats the
	// worst (key 15): tuple 2 evicted. A later WORSE row of tuple 2 (key
	// 30) must not re-enter; a later BETTER row (key 5) must.
	a := newArgminTopK(2, 1, 1, []bool{false})
	offer(a, 1, 10, 0)
	offer(a, 2, 20, 1)
	offer(a, 3, 15, 2)
	offer(a, 2, 30, 3)
	if got := render(a); got != fmt.Sprint([][]value.Value{{value.Int(1)}, {value.Int(3)}}) {
		t.Fatalf("after worse re-offer: %s, want tuples 1,3", got)
	}
	offer(a, 2, 5, 4)
	if got := render(a); got != fmt.Sprint([][]value.Value{{value.Int(2)}, {value.Int(1)}}) {
		t.Fatalf("after better re-entry: %s, want tuples 2,1", got)
	}

	// An improving row of a TRACKED candidate updates in place and the
	// heap reorders (tuple 1 improves past tuple 2).
	offer(a, 1, 3, 5)
	if got := render(a); got != fmt.Sprint([][]value.Value{{value.Int(1)}, {value.Int(2)}}) {
		t.Fatalf("after tracked improve: %s, want tuples 1,2", got)
	}

	// Key ties resolve by earlier sequence: a new tuple tying the worst
	// loses (its seq is larger).
	b := newArgminTopK(1, 1, 1, []bool{false})
	offer(b, 7, 10, 0)
	offer(b, 8, 10, 1)
	if got := render(b); got != fmt.Sprint([][]value.Value{{value.Int(7)}}) {
		t.Fatalf("tie at bound: %s, want tuple 7 (earlier seq)", got)
	}
}

// TestOrderedDistinctMatchesGeneral is the differential for the
// aggregate -> identity ORDER BY -> DISTINCT+LIMIT fusion: each fixture
// runs fused and pinned to the general materialize-sort-dedup pipeline
// (disableOrderedDistinct), and the ordered row lists must be identical.
//
// Injection-audit protocol (both degeneracy modes, catalog measurement
// item 7): re-apply a skip-every-third-group injection inside the argmin
// stream and check EVERY engaged fixture diverges; re-run when adding a
// fixture. Expected diverge set under that injection: every fixture with
// engage=true except LIMIT 0 (admits nothing either way).
func TestOrderedDistinctMatchesGeneral(t *testing.T) {
	g := colAggFixture(t)
	queries := []struct {
		q      string
		engage bool
	}{
		// Single-column DISTINCT over a repeated column (l repeats across
		// flag groups), aggregate-output order key. LIMIT near the
		// distinct count so an injected rank shift surfaces in output.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n DESC, l NEXT RETURN DISTINCT l AS ll LIMIT 20`, true},
		// Multi-column DISTINCT tuple (each tuple is one group, so a
		// dropped group removes a tuple outright).
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n DESC, l, f NEXT RETURN DISTINCT f, l LIMIT 30`, true},
		// Heavy key ties: the argmin tiebreak (group arrival) must match
		// the sort's index tiebreak exactly.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY f DESC NEXT RETURN DISTINCT l AS ll LIMIT 3`, true},
		// Non-column ORDER BY key expression.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY l % 7, n DESC, l NEXT RETURN DISTINCT l AS ll LIMIT 15`, true},
		// Rename on the ordering boundary (identity by position).
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN l AS a, n AS c ORDER BY c DESC, a NEXT RETURN DISTINCT a LIMIT 18`, true},
		// LIMIT past the distinct count (every rank shift visible), and
		// LIMIT 0 (admits nothing either way -- the audit's one expected
		// non-diverging engaged fixture).
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n, l NEXT RETURN DISTINCT l AS ll LIMIT 100`, true},
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n, l NEXT RETURN DISTINCT l AS ll LIMIT 0`, true},
		// SKIP consumes into the bound.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n DESC, l NEXT RETURN DISTINCT l AS ll SKIP 2 LIMIT 12`, true},
		// Declines: bounded ordering boundary (LIMIT truncates BEFORE the
		// dedup, a different result set).
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n DESC, l LIMIT 5 NEXT RETURN DISTINCT f AS ff LIMIT 2`, false},
		// Declines: computed DISTINCT item.
		{`MATCH (m:Message) RETURN m.length AS l, count(*) AS n NEXT RETURN l, n ORDER BY n DESC, l NEXT RETURN DISTINCT l + 1 AS l1 LIMIT 3`, false},
		// Declines: DISTINCT without LIMIT.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN l, f, n ORDER BY n DESC, l NEXT RETURN DISTINCT f AS ff`, false},
		// Declines: reordered (non-identity) ordering boundary.
		{`MATCH (m:Message) RETURN m.length AS l, m.flag AS f, count(*) AS n NEXT RETURN f, l, n ORDER BY n DESC, l NEXT RETURN DISTINCT f AS ff LIMIT 2`, false},
	}
	disableColAgg = true
	defer func() { disableColAgg = false }()
	for i, tc := range queries {
		before := orderedDistinctFusions
		fused := runOrdered(t, g, tc.q)
		fired := orderedDistinctFusions != before
		disableOrderedDistinct = true
		baseline := orderedDistinctFusions
		general := runOrdered(t, g, tc.q)
		disableOrderedDistinct = false
		if orderedDistinctFusions != baseline {
			t.Fatalf("query %d: disabled path still fused (switch dead)", i)
		}
		if fmt.Sprint(fused) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfused:   %v\ngeneral: %v", i, fused, general)
		}
		if fired != tc.engage {
			t.Errorf("query %d: fusion engagement = %v, want %v (vacuity guard)", i, fired, tc.engage)
		}
	}
}
