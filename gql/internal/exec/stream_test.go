package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// TestRowArena covers the bump-allocating row arena: copyRow retains a
// copy (not an alias) of a transient row, successive rows do not share
// backing, and rollback frees the most recent alloc so the next copyRow
// reuses that slot.
func TestRowArena(t *testing.T) {
	a := &rowArena{width: 3}

	src := []value.Value{value.Int(1), value.Int(2), value.Int(3)}
	r1 := a.copyRow(src)
	if len(r1) != 3 {
		t.Fatalf("copied row width = %d, want 3", len(r1))
	}
	// Mutating the source after the copy must not change the retained row.
	src[0] = value.Int(99)
	if v, _ := r1[0].AsInt(); v != 1 {
		t.Fatal("copyRow must copy, not alias, the source row")
	}

	// A second retained row does not share backing with the first.
	r2 := a.copyRow([]value.Value{value.Int(4), value.Int(5), value.Int(6)})
	r2[0] = value.Int(0)
	if v, _ := r1[0].AsInt(); v != 1 {
		t.Fatal("distinct arena rows must not share backing")
	}

	// rollback releases r2's slot; the next copyRow reuses it, so r2 (still
	// pointing at that slot) now reads the reused row's values.
	a.rollback()
	r3 := a.copyRow([]value.Value{value.Int(7), value.Int(8), value.Int(9)})
	if v, _ := r3[0].AsInt(); v != 7 {
		t.Fatalf("reused row = %v, want 7", r3[0])
	}
	if v, _ := r2[0].AsInt(); v != 7 {
		t.Fatal("rollback must free the slot for the next alloc to reuse")
	}
}
