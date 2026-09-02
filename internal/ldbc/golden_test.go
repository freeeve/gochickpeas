// Golden round-trip and drift reporting: FormatGolden/ParseGolden must
// invert each other section-for-section, and DiffGolden must name every
// drift class (changed, new, missing) with the subset switch
// suppressing only the missing check.
package ldbc

import (
	"slices"
	"testing"
)

func TestGoldenRoundTrip(t *testing.T) {
	entries := []GoldenEntry{
		{ID: "BI/Q1", Plan: "Segment 0\n  NodeScan (m:Message)\n  Aggregate"},
		{ID: "IC/IC9", Plan: "one line"},
		{ID: "SPB/a1", Plan: "Body with\n\ninterior blank line"},
	}
	parsed := ParseGolden(FormatGolden(entries))
	if len(parsed) != len(entries) {
		t.Fatalf("parsed %d sections, want %d", len(parsed), len(entries))
	}
	for _, e := range entries {
		if got := parsed[e.ID]; got != e.Plan {
			t.Fatalf("%s round-tripped to %q, want %q", e.ID, got, e.Plan)
		}
	}
	// Header lines and pre-section noise are ignored.
	if got := ParseGolden("# comment\nstray line\n\n=== X\nplan\n"); got["X"] != "plan" {
		t.Fatalf("prefix noise broke parsing: %v", got)
	}
}

func TestDiffGolden(t *testing.T) {
	golden := map[string]string{"a": "planA", "b": "planB", "gone": "planG"}
	current := []GoldenEntry{
		{ID: "a", Plan: "planA"},   // unchanged
		{ID: "b", Plan: "planB2"},  // changed
		{ID: "new", Plan: "planN"}, // not in golden
	}
	drift := DiffGolden(golden, current, false)
	want := []string{
		"b: plan shape changed",
		"gone: in golden but absent from this run",
		"new: new query, not in golden",
	}
	if !slices.Equal(drift, want) {
		t.Fatalf("drift = %v, want %v", drift, want)
	}
	// subset suppresses only the went-missing class.
	drift = DiffGolden(golden, current, true)
	want = []string{"b: plan shape changed", "new: new query, not in golden"}
	if !slices.Equal(drift, want) {
		t.Fatalf("subset drift = %v, want %v", drift, want)
	}
	if d := DiffGolden(golden, []GoldenEntry{{ID: "a", Plan: "planA"}}, true); len(d) != 0 {
		t.Fatalf("clean subset reported drift: %v", d)
	}
}
