// An absent property must read as absent regardless of the column's
// dense/sparse storage layout -- the correctness of IS NULL and every
// function over a missing value must not depend on how full the column
// happens to be. A dense str column stores a missing slot as atom 0 ("") and
// the read stack folds that to absent; numeric/bool columns only ever
// finalize dense at full coverage, so an in-range read is never a spurious
// present-zero. (rustychickpeas task 370 reported the divergence on the Rust
// side, where dense columns lacked presence tracking; this pins that
// gochickpeas does not share it -- task 184.)
package chickpeas

import "testing"

// buildFill stages span :N nodes, of which ids [0,present) carry v="plain"
// (string) and w=int64(7); the rest are absent.
func buildFill(t *testing.T, span, present int) *Snapshot {
	t.Helper()
	b := NewBuilder(span, 1)
	for i := 0; i < span; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < present; i++ {
		if err := b.SetProp(NodeID(i), "v", "plain"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(NodeID(i), "w", int64(7)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize()
}

// TestDenseStrAbsentReadsNull pins that a dense string column (85% fill, so
// it takes the dense layout) reads an in-range absent slot as absent through
// Prop.Str, not as the empty string it is physically stored as (task 184).
func TestDenseStrAbsentReadsNull(t *testing.T) {
	g := buildFill(t, 1000, 850)
	key, _ := g.atoms.ID("v")
	col := g.columns[PropertyKey(key)]
	if _, isDense := col.(denseStrCol); !isDense {
		t.Fatalf("v at 85%% fill should finalize dense str, got %T (test would not exercise the dense path)", col)
	}
	// A present slot reads its value; an absent in-range slot reads absent.
	if s, ok := g.Prop(NodeID(0), "v").Str(); !ok || s != "plain" {
		t.Fatalf("present dense-str slot: got (%q, %v), want (plain, true)", s, ok)
	}
	if s, ok := g.Prop(NodeID(900), "v").Str(); ok {
		t.Fatalf("absent dense-str slot read as present %q -- dense column lost presence (bug 184)", s)
	}
}

// TestNumericPartialNotDense pins that a partially-filled numeric column never
// takes the presence-less dense layout: it stays sparse/rank, so an absent
// slot reads absent instead of a spurious 0.
func TestNumericPartialNotDense(t *testing.T) {
	g := buildFill(t, 1000, 850)
	key, _ := g.atoms.ID("w")
	col := g.columns[PropertyKey(key)]
	if _, isDense := col.(denseI64Col); isDense {
		t.Fatal("partially-filled numeric column finalized dense -- an absent slot would read as 0 (bug 184)")
	}
	if v, ok := g.Prop(NodeID(0), "w").I64(); !ok || v != 7 {
		t.Fatalf("present numeric slot: got (%d, %v), want (7, true)", v, ok)
	}
	if v, ok := g.Prop(NodeID(900), "w").I64(); ok {
		t.Fatalf("absent numeric slot read as present %d -- expected absent (bug 184)", v)
	}
}
