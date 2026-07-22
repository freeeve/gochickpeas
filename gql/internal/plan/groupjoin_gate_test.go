package plan

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// labelScan is a one-stage prefix that scans all nodes of a label.
func labelScan(label string) []Stage {
	return []Stage{&MatchStage{Ops: []BindOp{{
		Kind:   OpScan,
		Source: ScanSource{Kind: ScanLabel, Label: label},
		Labels: []string{label},
	}}}}
}

// TestGjOuterBreadth covers the group-join outer-breadth estimate over the
// deterministic scan paths: an empty prefix is one row, a label scan is the
// label cardinality, an all-scan is the node count, and a carried-in
// ScanArg does not multiply the breadth.
func TestGjOuterBreadth(t *testing.T) {
	g := buildFixture(t)
	if b := gjOuterBreadth(nil, g); b != 1 {
		t.Fatalf("empty breadth = %v, want 1", b)
	}
	if b := gjOuterBreadth(labelScan("Person"), g); b != float64(g.LabelCardinality("Person")) {
		t.Fatalf("Person-scan breadth = %v, want %d", b, g.LabelCardinality("Person"))
	}
	all := []Stage{&MatchStage{Ops: []BindOp{{Kind: OpScan, Source: ScanSource{Kind: ScanAll}}}}}
	if b := gjOuterBreadth(all, g); b != float64(g.NodeCount()) {
		t.Fatalf("all-scan breadth = %v, want %d", b, g.NodeCount())
	}
	arg := []Stage{&MatchStage{Ops: []BindOp{{Kind: OpScan, Source: ScanSource{Kind: ScanArg}}}}}
	if b := gjOuterBreadth(arg, g); b != 1 {
		t.Fatalf("carried scan-arg breadth = %v, want 1 (not multiplied)", b)
	}
}

// gjBigGraph has L=1100 nodes (clears the 1024 outer-rows floor) and
// Big=2500 nodes, so a scan of L yields a breadth that covers L's own
// population but not Big's.
func gjBigGraph(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(4000, 0)
	for i := 0; i < 1100; i++ {
		if _, err := b.AddNode("L"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2500; i++ {
		if _, err := b.AddNode("Big"); err != nil {
			t.Fatal(err)
		}
	}
	return graphNew(b.Finalize())
}

// TestGjGate covers the economics gate: it declines below the absolute
// outer-rows floor, and for a passing floor it requires the breadth to cover
// (>= half) each correlation variable's population -- the corr label's own,
// the whole node count for an unlabeled corr var -- and passes when every
// corr label is covered or there are none.
func TestGjGate(t *testing.T) {
	// A 40-row Person outer is far below GroupJoinMinOuterRows (1024).
	if gjGate(&gjCandidate{corrLabels: []string{"Person"}}, labelScan("Person"), buildFixture(t)) {
		t.Fatal("a sub-floor outer breadth must not gate")
	}

	big := gjBigGraph(t)
	stages := labelScan("L") // breadth 1100, clears the floor

	// Covers L's own population (1100 >= 0.5*1100).
	if !gjGate(&gjCandidate{corrLabels: []string{"L"}}, stages, big) {
		t.Fatal("an outer covering the corr label must gate")
	}
	// No corr labels -> only the floor applies.
	if !gjGate(&gjCandidate{}, stages, big) {
		t.Fatal("no corr labels -> the floor alone gates")
	}
	// Big has 2500, needs >= 1250; a breadth of 1100 under-covers it.
	if gjGate(&gjCandidate{corrLabels: []string{"Big"}}, stages, big) {
		t.Fatal("an outer under-covering a corr label must not gate")
	}
	// An unlabeled corr var takes the whole node count (3600, needs 1800).
	if gjGate(&gjCandidate{corrLabels: []string{""}}, stages, big) {
		t.Fatal("an unlabeled corr var uses node count; must not gate when under-covered")
	}
}
