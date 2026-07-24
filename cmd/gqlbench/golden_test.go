package main

import (
	"fmt"
	"os"
	"testing"
)

// TestGoldenRoundTrip: a captured golden must parse back to exactly the plans
// it was built from, including multi-line plans with blank interior lines, so a
// capture-then-verify on unchanged plans never spuriously drifts.
func TestGoldenRoundTrip(t *testing.T) {
	entries := []goldenEntry{
		{id: "SPB/q1", plan: "NodeScan (a:Tag)\n  [anchor: a:Tag card=N]\nProject [a.name]"},
		{id: "BI/Q8", plan: "NodeByProperty (p:Person {id = N})\nExpand (p)-[:KNOWS]->(f)\nAggregate (group=[x]; count(f))"},
		{id: "IC/3", plan: "NodeScan (n:City)\nProject [n.name]"},
	}
	got := parseGolden(formatGolden(entries))
	if len(got) != len(entries) {
		t.Fatalf("round-trip lost entries: got %d, want %d", len(got), len(entries))
	}
	for _, e := range entries {
		if got[e.id] != e.plan {
			t.Fatalf("round-trip mismatch for %s:\n got %q\nwant %q", e.id, got[e.id], e.plan)
		}
	}
}

// TestDiffGoldenDetectsDrift: the diff must flag a changed plan, a new query,
// and a query that vanished -- and stay silent when every plan is identical.
func TestDiffGoldenDetectsDrift(t *testing.T) {
	base := []goldenEntry{
		{id: "A", plan: "NodeScan (a)\nProject [a]"},
		{id: "B", plan: "NodeScan (b)\nProject [b]"},
	}
	golden := parseGolden(formatGolden(base))

	if d := diffGolden(golden, base, false); len(d) != 0 {
		t.Fatalf("identical plans reported drift: %v", d)
	}

	changed := []goldenEntry{
		{id: "A", plan: "NodeByProperty (a {k = N})\nProject [a]"}, // A's plan moved
		// B missing entirely
		{id: "C", plan: "NodeScan (c)\nProject [c]"}, // C is new
	}
	d := diffGolden(golden, changed, false)
	joined := fmt.Sprint(d)
	for _, want := range []string{"A: plan shape changed", "C: new query", "B: in golden but absent"} {
		if !contains(d, want) {
			t.Fatalf("expected drift %q in %s", want, joined)
		}
	}

	// A subset run (-only) says nothing about queries it never planned:
	// the absence line disappears while real drift still reports.
	ds := diffGolden(golden, changed, true)
	if contains(ds, "B: in golden but absent") {
		t.Fatalf("subset diff reported an absence line: %v", ds)
	}
	for _, want := range []string{"A: plan shape changed", "C: new query"} {
		if !contains(ds, want) {
			t.Fatalf("subset diff lost real drift %q in %v", want, ds)
		}
	}
}

// TestGoldenFileWellFormed guards the committed plan-shape corpus in plain CI
// (no manifest, no graph load): it must parse to the manifest's 89 queries and
// every plan must round-trip through format+parse unchanged, so a later capture
// writes a byte-stable file and verify never spuriously drifts on a corrupted
// or hand-edited golden.
func TestGoldenFileWellFormed(t *testing.T) {
	data, err := os.ReadFile("testdata/plans_golden.txt")
	if err != nil {
		t.Fatalf("read committed golden: %v", err)
	}
	entries := parseGolden(string(data))
	if len(entries) != 89 {
		t.Fatalf("committed golden has %d queries, want 89 (regenerate with -plans-golden-capture if the manifest changed)", len(entries))
	}
	var ordered []goldenEntry
	for id, plan := range entries {
		ordered = append(ordered, goldenEntry{id: id, plan: plan})
	}
	round := parseGolden(formatGolden(ordered))
	for id, plan := range entries {
		if round[id] != plan {
			t.Fatalf("committed golden entry %s did not round-trip through format+parse", id)
		}
	}
}

// contains reports whether any element of xs has want as a prefix.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if len(x) >= len(want) && x[:len(want)] == want {
			return true
		}
	}
	return false
}
