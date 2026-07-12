// Typed column reader tests: the contiguous-presence dense window.
package chickpeas

import "testing"

// TestSliceRange pins the contiguous-presence dense window (the rcp
// as_i64_slice_range twin): dense columns window from 0, a sparse column
// over one contiguous id run windows at its start with the value array
// itself, and gapped presence declines.
func TestSliceRange(t *testing.T) {
	b := NewBuilder(64, 0)
	for i := 0; i < 64; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	// Contiguous block [10, 20).
	for i := 10; i < 20; i++ {
		if err := b.SetProp(NodeID(i), "blk", int64(i*7)); err != nil {
			t.Fatal(err)
		}
	}
	// Gapped presence.
	for _, i := range []int{3, 5, 9} {
		if err := b.SetProp(NodeID(i), "gap", int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	blk, _ := g.Col("blk")
	start, vals, ok := blk.I64().SliceRange()
	if !ok || start != 10 || len(vals) != 10 {
		t.Fatalf("blk window = (%d, %d, %v), want (10, 10, true)", start, len(vals), ok)
	}
	for i := 10; i < 20; i++ {
		if vals[i-int(start)] != int64(i*7) {
			t.Fatalf("blk[%d] = %d, want %d", i, vals[i-int(start)], i*7)
		}
	}
	gap, _ := g.Col("gap")
	if _, _, ok := gap.I64().SliceRange(); ok {
		t.Fatal("gapped presence must decline the window")
	}
	if _, _, ok := gap.F64().SliceRange(); ok {
		t.Fatal("wrong dtype must decline the window")
	}
}
