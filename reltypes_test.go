package chickpeas

import (
	"fmt"
	"testing"
)

// narrowGraph: 4 nodes, two rel types, palette (narrow) representation.
// n0 -KNOWS-> n1, n0 -LIKES-> n2, n1 -KNOWS-> n3.
func narrowGraph(t *testing.T) *Snapshot {
	t.Helper()
	b := NewBuilder(4, 4)
	for i := 0; i < 4; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		u, v NodeID
		ty   string
	}{{0, 1, "KNOWS"}, {0, 2, "LIKES"}, {1, 3, "KNOWS"}} {
		if _, err := b.AddRel(r.u, r.v, r.ty); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("narrow")
}

// wideGraph exceeds the palette cap so the type vectors keep the wide
// []RelType representation: node 0 fans out one rel each of 300 types.
func wideGraph(t *testing.T) *Snapshot {
	t.Helper()
	b := NewBuilder(2, relTypePaletteMax+64)
	if _, err := b.AddNode("N"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNode("N"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < relTypePaletteMax+44; i++ {
		if _, err := b.AddRel(0, 1, fmt.Sprintf("T%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("wide")
}

// countMatches drives typeTest over every out position of the graph.
func countMatches(g *Snapshot, m RelMatch) int {
	keep := typeTest(m, &g.outTypes, true)
	n := 0
	for k := 0; k < g.outTypes.Len(); k++ {
		if keep(uint32(k)) {
			n++
		}
	}
	return n
}

func TestTypeTestNarrow(t *testing.T) {
	g := narrowGraph(t)
	if g.outTypes.wide != nil {
		t.Fatal("expected narrow (palette) representation")
	}
	if got := countMatches(g, MatchAll()); got != 3 {
		t.Fatalf("MatchAll matched %d, want 3", got)
	}
	if got := countMatches(g, g.Match("KNOWS")); got != 2 {
		t.Fatalf("KNOWS matched %d, want 2", got)
	}
	if got := countMatches(g, g.Match("LIKES")); got != 1 {
		t.Fatalf("LIKES matched %d, want 1", got)
	}
	if got := countMatches(g, g.Match("KNOWS", "LIKES")); got != 3 {
		t.Fatalf("KNOWS|LIKES matched %d, want 3", got)
	}
	if got := countMatches(g, MatchNone()); got != 0 {
		t.Fatalf("MatchNone matched %d, want 0", got)
	}
	// Snapshot-less matcher: no typed holder, palette scan path.
	kt, _ := g.RelType("KNOWS")
	if got := countMatches(g, MatchType(kt)); got != 2 {
		t.Fatalf("MatchType(KNOWS) matched %d, want 2", got)
	}
	// A type absent from the graph matches nothing.
	if got := countMatches(g, MatchType(RelType(999999))); got != 0 {
		t.Fatalf("absent type matched %d, want 0", got)
	}
}

func TestTypeTestWide(t *testing.T) {
	g := wideGraph(t)
	if g.outTypes.wide == nil {
		t.Fatal("expected wide representation past the palette cap")
	}
	if got := countMatches(g, MatchAll()); got != relTypePaletteMax+44 {
		t.Fatalf("MatchAll matched %d", got)
	}
	if got := countMatches(g, g.Match("T007")); got != 1 {
		t.Fatalf("T007 matched %d, want 1", got)
	}
	if got := countMatches(g, g.Match("T007", "T123", "T299")); got != 3 {
		t.Fatalf("3-type matched %d, want 3", got)
	}
	if got := countMatches(g, MatchType(RelType(999999))); got != 0 {
		t.Fatalf("absent type matched %d, want 0", got)
	}
}

func TestAppendAndYieldNbrsTypedAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *Snapshot
	}{{"narrow", narrowGraph(t)}, {"wide", wideGraph(t)}} {
		g := tc.g
		for _, m := range []RelMatch{MatchAll(), g.Match(g.RelTypes()[0]), g.Match(g.RelTypes()...), MatchNone()} {
			lo, hi := 0, g.outTypes.Len()
			direct := appendNbrsTyped(nil, g.outNbrs, &g.outTypes, m, true, lo, hi)
			var yielded []NodeID
			yieldNbrsTyped(g.outNbrs, &g.outTypes, m, true, lo, hi, func(n NodeID) bool {
				yielded = append(yielded, n)
				return true
			})
			if len(direct) != len(yielded) {
				t.Fatalf("%s: append %d vs yield %d neighbors", tc.name, len(direct), len(yielded))
			}
			for i := range direct {
				if direct[i] != yielded[i] {
					t.Fatalf("%s: divergence at %d: %d vs %d", tc.name, i, direct[i], yielded[i])
				}
			}
			// Early stop after the first neighbor.
			seen := 0
			yieldNbrsTyped(g.outNbrs, &g.outTypes, m, true, lo, hi, func(NodeID) bool {
				seen++
				return false
			})
			if len(direct) > 0 && seen != 1 {
				t.Fatalf("%s: early stop yielded %d, want 1", tc.name, seen)
			}
		}
	}
}

func TestSchemaIDMatchesAtomLookup(t *testing.T) {
	g := narrowGraph(t)
	for _, name := range []string{"N", "KNOWS", "LIKES"} {
		fast, fok := g.schemaID(name)
		slow, sok := g.atoms.ID(name)
		if fast != slow || fok != sok {
			t.Fatalf("schemaID(%q) = (%d,%v), atoms.ID = (%d,%v)", name, fast, fok, slow, sok)
		}
	}
	if _, ok := g.schemaID("NEVER_INTERNED"); ok {
		t.Fatal("unknown name resolved")
	}
}
