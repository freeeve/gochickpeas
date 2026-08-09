package exec

import (
	"fmt"
	"testing"
)

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
