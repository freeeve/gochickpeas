package exec

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestPathRelPositionsOf covers reading a hop's bound relationship value: a
// single rel (fixed hop), a list (quantified hop, non-rel elements dropped),
// and a non-rel value (nil).
func TestPathRelPositionsOf(t *testing.T) {
	if got := pathRelPositionsOf(value.Rel(5)); !slices.Equal(got, []uint32{5}) {
		t.Fatalf("single rel = %v, want [5]", got)
	}
	list := value.List([]value.Value{value.Rel(2), value.Rel(7)})
	if got := pathRelPositionsOf(list); !slices.Equal(got, []uint32{2, 7}) {
		t.Fatalf("rel list = %v, want [2 7]", got)
	}
	// Non-rel elements in the list are dropped.
	mixed := value.List([]value.Value{value.Rel(2), value.Int(3), value.Rel(7)})
	if got := pathRelPositionsOf(mixed); !slices.Equal(got, []uint32{2, 7}) {
		t.Fatalf("mixed list = %v, want [2 7]", got)
	}
	if got := pathRelPositionsOf(value.Int(9)); got != nil {
		t.Fatalf("non-rel value = %v, want nil", got)
	}
}

// relPos returns the CSR position of the from->to relationship of type ty.
func relPos(t *testing.T, sg *graph.SnapshotGraph, from, to chickpeas.NodeID, ty string) uint32 {
	t.Helper()
	for n, p := range sg.Relationships(from, chickpeas.Outgoing, []string{ty}) {
		if n == to {
			return p
		}
	}
	t.Fatalf("no %s rel %d->%d", ty, from, to)
	return 0
}

// TestReconstructPathNodes covers walking a start node through bound
// relationship positions: forward and reverse (each rel resolves from either
// endpoint), the empty walk, a position not incident to the current node
// (returns the prefix), and an invalid position (breaks).
func TestReconstructPathNodes(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	a, _ := b.AddNode("N")
	mid, _ := b.AddNode("N")
	c, _ := b.AddNode("N")
	if _, err := b.AddRel(a, mid, "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(mid, c, "R"); err != nil {
		t.Fatal(err)
	}
	sg := graph.New(b.Finalize("path"))
	ctx := &eval.Ctx{G: sg}
	posAM := relPos(t, sg, a, mid, "R")
	posMC := relPos(t, sg, mid, c, "R")

	// Forward: a -R-> mid -R-> c.
	if got := reconstructPathNodes(ctx, a, []uint32{posAM, posMC}); !slices.Equal(got, []chickpeas.NodeID{a, mid, c}) {
		t.Fatalf("forward walk = %v, want [%d %d %d]", got, a, mid, c)
	}
	// Reverse: c walked back through the same rels resolves from the far
	// endpoint each hop.
	if got := reconstructPathNodes(ctx, c, []uint32{posMC, posAM}); !slices.Equal(got, []chickpeas.NodeID{c, mid, a}) {
		t.Fatalf("reverse walk = %v, want [%d %d %d]", got, c, mid, a)
	}
	// Empty rel list: just the start node.
	if got := reconstructPathNodes(ctx, a, nil); !slices.Equal(got, []chickpeas.NodeID{a}) {
		t.Fatalf("empty walk = %v, want [%d]", got, a)
	}
	// A relationship not incident to the current node stops the walk at the
	// prefix built so far.
	if got := reconstructPathNodes(ctx, a, []uint32{posMC}); !slices.Equal(got, []chickpeas.NodeID{a}) {
		t.Fatalf("disconnected walk = %v, want [%d]", got, a)
	}
	// An out-of-range position (no endpoints) breaks the walk.
	if got := reconstructPathNodes(ctx, a, []uint32{1 << 20}); !slices.Equal(got, []chickpeas.NodeID{a}) {
		t.Fatalf("invalid-position walk = %v, want [%d]", got, a)
	}
}
