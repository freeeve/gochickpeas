package plan

import "testing"

// TestScanLabelAcrossMatchReuse locks the anchor-scan bounding when a
// variable is labeled in one MATCH and re-used bare in another (task 215, a
// cross-engine check from ragedb whose recognizer missed this). Cost-based
// spec reordering anchors on the labeled node regardless of clause order, so
// the start-node scan is a bounded ScanLabel whenever the variable carries a
// label in ANY match, and only falls back to the unbounded ScanAll when no
// match spells a label -- the genuine decline boundary. This is an invisible
// (plan-shape, not result) optimization, so it needs a plan-shape assertion.
func TestScanLabelAcrossMatchReuse(t *testing.T) {
	g := buildFixture(t)
	scan := func(src string) ScanSource {
		return firstMatch(t, mustPlan(t, g, src)).Ops[0].Source
	}

	// Single-match baseline: the label bounds the scan.
	if s := scan("MATCH (s:Person)-[:KNOWS]->(t) RETURN s"); s.Kind != ScanLabel || s.Label != "Person" {
		t.Fatalf("baseline scan = kind %v label %q, want ScanLabel Person", s.Kind, s.Label)
	}
	// Forward re-use: labeled first, re-used bare in a later match.
	if s := scan("MATCH (s:Person) MATCH (s)-[:KNOWS]->(t) RETURN s"); s.Kind != ScanLabel || s.Label != "Person" {
		t.Fatalf("forward re-use scan = kind %v label %q, want ScanLabel Person", s.Kind, s.Label)
	}
	// Reverse re-use: used bare first, labeled only in a later match --
	// reordering still hoists the labeled anchor.
	if s := scan("MATCH (s)-[:KNOWS]->(t) MATCH (s:Person) RETURN s"); s.Kind != ScanLabel || s.Label != "Person" {
		t.Fatalf("reverse re-use scan = kind %v label %q, want ScanLabel Person", s.Kind, s.Label)
	}
	// A LIMIT does not disturb the label bounding.
	if s := scan("MATCH (s:Person) MATCH (s)-[:KNOWS]->(t) RETURN s LIMIT 5"); s.Kind != ScanLabel || s.Label != "Person" {
		t.Fatalf("forward+LIMIT scan = kind %v label %q, want ScanLabel Person", s.Kind, s.Label)
	}
	// The genuine boundary: the variable is never labeled anywhere, so the
	// scan stays unbounded.
	if s := scan("MATCH (s) MATCH (s)-[:KNOWS]->(t) RETURN s"); s.Kind != ScanAll {
		t.Fatalf("never-labeled scan = kind %v, want ScanAll", s.Kind)
	}
}
