// Range-index tests: window bounds (inclusive, exclusive, unbounded
// sentinels, inverted, empty), (value, id) tie ordering, dense and sparse
// column sources, extreme values, and the miss cases (unknown key, wrong
// dtype, rel-only key).
package chickpeas

import (
	"math"
	"slices"
	"testing"
)

func TestColRangeIndexWindows(t *testing.T) {
	b := NewBuilder(64, 4)
	for i := 0; i < 40; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Sparse i64 column with ties, negatives, and extremes; nodes 20+
	// carry nothing.
	vals := map[NodeID]int64{
		0: 10, 1: -5, 2: 10, 3: 7, 4: math.MaxInt64, 5: math.MinInt64,
		6: 10, 7: 0, 8: -5, 9: 42,
	}
	for id, v := range vals {
		must(b.SetProp(id, "ts", v))
	}
	// A string column and a rel-only key for the miss cases.
	must(b.SetProp(0, "name", "zero"))
	r, err := b.AddRel(0, 1, "R")
	must(err)
	must(b.SetRelPropAt(r, "w", int64(9)))
	g := b.Finalize()

	ri, ok := g.ColRangeIndex("ts")
	if !ok || ri.Len() != len(vals) {
		t.Fatalf("ColRangeIndex(ts) = (len %d, %v), want (%d, true)", ri.Len(), ok, len(vals))
	}
	window := func(lo, hi int64, loIncl, hiIncl bool) []uint32 {
		w := ri.Window(lo, hi, loIncl, hiIncl)
		out := append([]uint32(nil), w...)
		return out
	}
	// Ties (three 10s) enumerate in ascending id order.
	if got := window(10, 10, true, true); !slices.Equal(got, []uint32{0, 2, 6}) {
		t.Fatalf("[10,10] = %v, want [0 2 6]", got)
	}
	// Half-open bounds.
	if got := window(0, 10, false, false); !slices.Equal(got, []uint32{3}) {
		t.Fatalf("(0,10) = %v, want [3]", got)
	}
	if got := window(0, 10, true, false); !slices.Equal(got, []uint32{7, 3}) {
		t.Fatalf("[0,10) = %v, want [7 3]", got)
	}
	// Unbounded sentinels cover the extremes.
	all := window(math.MinInt64, math.MaxInt64, true, true)
	if len(all) != len(vals) {
		t.Fatalf("full window = %d entries, want %d", len(all), len(vals))
	}
	if all[0] != 5 || all[len(all)-1] != 4 {
		t.Fatalf("full window ends = (%d, %d), want (5, 4)", all[0], all[len(all)-1])
	}
	// Exclusive bounds at the extremes drop them.
	if got := window(math.MinInt64, math.MaxInt64, false, false); len(got) != len(vals)-2 {
		t.Fatalf("open full window = %d entries, want %d", len(got), len(vals)-2)
	}
	// Empty and inverted intervals.
	if got := window(11, 41, true, true); got != nil {
		t.Fatalf("[11,41] = %v, want empty", got)
	}
	if got := window(10, 0, true, true); got != nil {
		t.Fatalf("inverted = %v, want empty", got)
	}
	// Misses: unknown key, wrong dtype, rel-only key.
	if _, ok := g.ColRangeIndex("nope"); ok {
		t.Fatal("unknown key built an index")
	}
	if _, ok := g.ColRangeIndex("name"); ok {
		t.Fatal("string column built an index")
	}
	if _, ok := g.ColRangeIndex("w"); ok {
		t.Fatal("rel-only key built a node index")
	}
	// The cache returns the same backing on re-resolve.
	ri2, _ := g.ColRangeIndex("ts")
	if &ri.ids[0] != &ri2.ids[0] {
		t.Fatal("second resolve rebuilt the index")
	}
}

func TestColRangeIndexDense(t *testing.T) {
	// A full column goes dense at Finalize; the index must read it the
	// same way.
	b := NewBuilder(16, 0)
	for i := 0; i < 16; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(NodeID(i), "v", int64(15-i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()
	ri, ok := g.ColRangeIndex("v")
	if !ok || ri.Len() != 16 {
		t.Fatalf("dense index = (len %d, %v), want (16, true)", ri.Len(), ok)
	}
	if got := ri.Window(0, 3, true, true); !slices.Equal(got, []uint32{15, 14, 13, 12}) {
		t.Fatalf("[0,3] = %v, want [15 14 13 12]", got)
	}
}
