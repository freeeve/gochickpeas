// Rank-column reader tests: the moderately-sparse band's O(1) typed columns
// (i64/f64/bool/str). Each is built by Finalize only when a property's density
// falls in the rank band, which the small in-memory fixtures elsewhere never
// hit -- so construct them directly over a known sparse layout and pin Get
// (present, absent, and out-of-span positions), Entries (ascending-position
// order with slot-aligned values), Dtype, and Len.
package chickpeas

import (
	"testing"

	"github.com/freeeve/gochickpeas/internal/bitset"
)

// rankPositions is the shared sparse layout: three values at positions 2, 5, 9
// within a 12-position span, so slot i corresponds to positions[i].
var rankPositions = []uint32{2, 5, 9}

const rankSpan = 12

// collectEntries drains a rank column's Entries into parallel position/value
// slices for order-sensitive assertions.
func collectEntries(c Column) ([]uint32, []Value) {
	var poss []uint32
	var vals []Value
	for pos, v := range c.Entries() {
		poss = append(poss, pos)
		vals = append(vals, v)
	}
	return poss, vals
}

// assertPositions checks that a rank column's Entries yielded exactly the
// sparse layout's positions, in ascending order.
func assertPositions(t *testing.T, got []uint32) {
	t.Helper()
	if len(got) != len(rankPositions) {
		t.Fatalf("Entries yielded %d positions, want %d", len(got), len(rankPositions))
	}
	for i, p := range rankPositions {
		if got[i] != p {
			t.Fatalf("Entries position %d = %d, want %d (ascending layout order)", i, got[i], p)
		}
	}
}

func TestRankI64Col(t *testing.T) {
	c := rankI64Col{
		rankIndex: buildRankIndex(rankPositions, rankSpan),
		vals:      []int64{10, 20, 30},
	}
	if c.Dtype() != DtypeI64 {
		t.Fatalf("Dtype = %v, want DtypeI64", c.Dtype())
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	// Present positions read their slot value.
	for i, pos := range rankPositions {
		v, ok := c.Get(pos)
		if !ok {
			t.Fatalf("Get(%d) missing, want present", pos)
		}
		if iv, _ := v.I64(); iv != c.vals[i] {
			t.Fatalf("Get(%d) = %d, want %d", pos, iv, c.vals[i])
		}
	}
	// Absent positions (interior gaps) and out-of-span positions are not-ok.
	for _, pos := range []uint32{0, 3, 8, 11, 100} {
		if _, ok := c.Get(pos); ok {
			t.Fatalf("Get(%d) present, want absent", pos)
		}
	}
	poss, vals := collectEntries(c)
	assertPositions(t, poss)
	for i := range vals {
		if iv, _ := vals[i].I64(); iv != c.vals[i] {
			t.Fatalf("Entries value %d = %d, want %d", i, iv, c.vals[i])
		}
	}
}

func TestRankF64Col(t *testing.T) {
	c := rankF64Col{
		rankIndex: buildRankIndex(rankPositions, rankSpan),
		vals:      []float64{1.5, 2.5, 3.5},
	}
	if c.Dtype() != DtypeF64 {
		t.Fatalf("Dtype = %v, want DtypeF64", c.Dtype())
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for i, pos := range rankPositions {
		v, ok := c.Get(pos)
		if !ok {
			t.Fatalf("Get(%d) missing, want present", pos)
		}
		if fv, _ := v.F64(); fv != c.vals[i] {
			t.Fatalf("Get(%d) = %v, want %v", pos, fv, c.vals[i])
		}
	}
	for _, pos := range []uint32{0, 4, 11, 100} {
		if _, ok := c.Get(pos); ok {
			t.Fatalf("Get(%d) present, want absent", pos)
		}
	}
	poss, vals := collectEntries(c)
	assertPositions(t, poss)
	for i := range vals {
		if fv, _ := vals[i].F64(); fv != c.vals[i] {
			t.Fatalf("Entries value %d = %v, want %v", i, fv, c.vals[i])
		}
	}
}

func TestRankBoolCol(t *testing.T) {
	// vals is slot-indexed: slot 0 -> true, slot 1 -> false, slot 2 -> true.
	bits := bitset.New(3)
	bits.Set(0, true)
	bits.Set(2, true)
	c := rankBoolCol{
		rankIndex: buildRankIndex(rankPositions, rankSpan),
		vals:      bits,
	}
	if c.Dtype() != DtypeBool {
		t.Fatalf("Dtype = %v, want DtypeBool", c.Dtype())
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	want := []bool{true, false, true}
	for i, pos := range rankPositions {
		v, ok := c.Get(pos)
		if !ok {
			t.Fatalf("Get(%d) missing, want present", pos)
		}
		if bv, _ := v.Bool(); bv != want[i] {
			t.Fatalf("Get(%d) = %v, want %v", pos, bv, want[i])
		}
	}
	if _, ok := c.Get(7); ok {
		t.Fatalf("Get(7) present, want absent")
	}
	poss, vals := collectEntries(c)
	assertPositions(t, poss)
	for i := range vals {
		if bv, _ := vals[i].Bool(); bv != want[i] {
			t.Fatalf("Entries value %d = %v, want %v", i, bv, want[i])
		}
	}
}

func TestRankStrCol(t *testing.T) {
	c := rankStrCol{
		rankIndex: buildRankIndex(rankPositions, rankSpan),
		vals:      []uint32{100, 200, 300},
	}
	if c.Dtype() != DtypeStr {
		t.Fatalf("Dtype = %v, want DtypeStr", c.Dtype())
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for i, pos := range rankPositions {
		v, ok := c.Get(pos)
		if !ok {
			t.Fatalf("Get(%d) missing, want present", pos)
		}
		if av, _ := v.StrID(); av != c.vals[i] {
			t.Fatalf("Get(%d) atom = %d, want %d", pos, av, c.vals[i])
		}
	}
	for _, pos := range []uint32{1, 6, 100} {
		if _, ok := c.Get(pos); ok {
			t.Fatalf("Get(%d) present, want absent", pos)
		}
	}
	poss, vals := collectEntries(c)
	assertPositions(t, poss)
	for i := range vals {
		if av, _ := vals[i].StrID(); av != c.vals[i] {
			t.Fatalf("Entries value %d atom = %d, want %d", i, av, c.vals[i])
		}
	}
}
