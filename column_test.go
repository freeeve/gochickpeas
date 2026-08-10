package chickpeas

import (
	"testing"
)

// TestNarrowI64ColumnClasses pins the storage-class chooser and value
// identity: every class reads back exactly the values a plain []int64
// column would, through Get, Entries, and the typed I64Col reader --
// narrowing is a storage choice, never a value change. Spans sit at the
// class boundaries (including negative minimums) and the below-threshold
// case stays plain.
func TestNarrowI64ColumnClasses(t *testing.T) {
	mk := func(min, span int64) []int64 {
		vals := make([]int64, narrowI64MinLen+3)
		for i := range vals {
			vals[i] = min + int64(i)%(span+1)
		}
		vals[len(vals)-1] = min + span // span endpoint present
		return vals
	}
	cases := []struct {
		name      string
		vals      []int64
		wantClass string
	}{
		{"u8 span", mk(-40, 0xFF), "narrow1"},
		{"u16 boundary", mk(1_000_000, 0xFFFF), "narrow2"},
		{"u32 boundary", mk(-2_000_000_000, 0xFFFFFFFF), "narrow4"},
		{"u48 lower edge", mk(0, 0x1_0000_0000), "narrow6"},
		{"u48 timestamps", mk(1_263_065_046_975, 91_092_451_239), "narrow6"},
		{"u48 boundary", mk(-1_000, 0xFFFF_FFFF_FFFF), "narrow6"},
		{"too wide", mk(0, 0x1_0000_0000_0000), "dense"},
		{"below threshold stays plain", []int64{1_263_065_046_975, 1_354_157_498_214}, "dense"},
		// Two-valued columns (0/1 flags, or any {min, min+1} pair) pack
		// to one bit per position.
		{"zero-one flags", mk(0, 1), "bit"},
		{"offset two-valued", mk(41, 1), "bit"},
		{"constant column", mk(7, 0), "bit"},
	}
	for _, tc := range cases {
		col := narrowI64Column(tc.vals)
		class := "dense"
		switch n := col.(type) {
		case denseI64NarrowCol:
			class = map[uint8]string{1: "narrow1", 2: "narrow2", 4: "narrow4", 6: "narrow6"}[n.w]
		case denseI64BitCol:
			class = "bit"
		}
		if class != tc.wantClass {
			t.Fatalf("%s: class = %s, want %s", tc.name, class, tc.wantClass)
		}
		if col.Len() != len(tc.vals) {
			t.Fatalf("%s: Len = %d, want %d", tc.name, col.Len(), len(tc.vals))
		}
		if col.Dtype() != DtypeI64 {
			t.Fatalf("%s: Dtype = %v, want I64", tc.name, col.Dtype())
		}
		for i, want := range tc.vals {
			v, ok := col.Get(uint32(i))
			if !ok {
				t.Fatalf("%s: Get(%d) absent", tc.name, i)
			}
			if got, _ := v.I64(); got != want {
				t.Fatalf("%s: Get(%d) = %d, want %d", tc.name, i, got, want)
			}
		}
		if _, ok := col.Get(uint32(len(tc.vals))); ok {
			t.Fatalf("%s: Get past end reported present", tc.name)
		}
		i := 0
		for pos, v := range col.Entries() {
			got, _ := v.I64()
			if int(pos) != i || got != tc.vals[i] {
				t.Fatalf("%s: Entries[%d] = (%d, %d), want (%d, %d)", tc.name, i, pos, got, i, tc.vals[i])
			}
			i++
		}
		if i != len(tc.vals) {
			t.Fatalf("%s: Entries yielded %d, want %d", tc.name, i, len(tc.vals))
		}
		// The typed reader's narrow fast path agrees with Get.
		r := Col{col: col}.I64()
		for i, want := range tc.vals {
			got, ok := r.Get(uint32(i))
			if !ok || got != want {
				t.Fatalf("%s: I64Col.Get(%d) = (%d, %v), want (%d, true)", tc.name, i, got, ok, want)
			}
		}
		if _, ok := r.Get(uint32(len(tc.vals))); ok {
			t.Fatalf("%s: I64Col.Get past end reported present", tc.name)
		}
		// Slice stays a plain-dense-only contract.
		if _, ok := r.Slice(); ok != (class == "dense") {
			t.Fatalf("%s: Slice ok = %v, want %v", tc.name, ok, class == "dense")
		}
	}
}

// TestNarrowI64SparseAndRank pins the narrow value store under the sparse
// and rank layouts: columnFromPairsI64 narrows their value arrays when the
// span fits, and Get/Entries/valueAtSlot/serialization read back the exact
// values the wide layouts would.
func TestNarrowI64SparseAndRank(t *testing.T) {
	// Sparse: gappy positions (every 40th) over a wide span defeat the
	// dense and rank thresholds; values sit in a u16 range.
	n := narrowI64MinLen + 5
	ids := make([]uint32, n)
	pairs := make([]i64Pair, n)
	for i := range n {
		ids[i] = uint32(i * 40)
		pairs[i] = i64Pair{id: ids[i], val: 7_000 + int64(i%900)}
	}
	span := int(ids[n-1]) + 1
	col := columnFromPairsI64(pairs, span)
	sn, ok := col.(sparseI64NarrowCol)
	if !ok {
		t.Fatalf("sparse fixture built %T, want sparseI64NarrowCol", col)
	}
	if sn.w != 2 {
		t.Fatalf("sparse narrow width = %d, want 2", sn.w)
	}
	for i := range n {
		v, present := col.Get(ids[i])
		if !present {
			t.Fatalf("sparse Get(%d) absent", ids[i])
		}
		if got, _ := v.I64(); got != pairs[i].val {
			t.Fatalf("sparse Get(%d) = %d, want %d", ids[i], got, pairs[i].val)
		}
		sv, present := valueAtSlot(col, uint32(i))
		if got, _ := sv.I64(); !present || got != pairs[i].val {
			t.Fatalf("valueAtSlot(%d) = (%d, %v), want (%d, true)", i, got, present, pairs[i].val)
		}
	}
	if _, present := col.Get(1); present {
		t.Fatal("sparse Get on absent position reported present")
	}
	if !isSparse(col) {
		t.Fatal("sparseI64NarrowCol not classified sparse (posIndex path)")
	}
	back := dataToColumn(columnToData(col))
	if _, ok := back.(sparseI64NarrowCol); !ok {
		t.Fatalf("sparse round trip lost narrowing: %T", back)
	}
	for i := range n {
		v, _ := back.Get(ids[i])
		if got, _ := v.I64(); got != pairs[i].val {
			t.Fatalf("sparse round trip Get(%d) = %d, want %d", ids[i], got, pairs[i].val)
		}
	}

	// Rank: the layout needs span >= 1M (rankSelectMinLen) with presence
	// dense enough that presence bits + values beat sparse pairs
	// (span <= 64*present); every 8th position over 1.6M qualifies.
	rn := 200_000
	rpairs := make([]i64Pair, rn)
	for i := range rn {
		rpairs[i] = i64Pair{id: uint32(i * 8), val: int64(i % 250)}
	}
	rcol := columnFromPairsI64(rpairs, rn*8)
	rk, ok := rcol.(rankI64NarrowCol)
	if !ok {
		t.Fatalf("rank fixture built %T, want rankI64NarrowCol", rcol)
	}
	if rk.w != 1 {
		t.Fatalf("rank narrow width = %d, want 1", rk.w)
	}
	for i := 0; i < rn; i += 97 {
		v, present := rcol.Get(uint32(i * 8))
		if !present {
			t.Fatalf("rank Get(%d) absent", i*8)
		}
		if got, _ := v.I64(); got != int64(i%250) {
			t.Fatalf("rank Get(%d) = %d, want %d", i*8, got, i%250)
		}
	}
	if _, present := rcol.Get(1); present {
		t.Fatal("rank Get on absent position reported present")
	}
}

// TestNarrowI64SerializeRoundTrip pins that a narrowed column serializes
// width-agnostically (logical values) and re-narrows on load with values
// intact.
func TestNarrowI64SerializeRoundTrip(t *testing.T) {
	vals := make([]int64, narrowI64MinLen)
	for i := range vals {
		vals[i] = 500 + int64(i%300)
	}
	col := narrowI64Column(vals)
	if _, ok := col.(denseI64NarrowCol); !ok {
		t.Fatal("fixture did not narrow")
	}
	back := dataToColumn(columnToData(col))
	if _, ok := back.(denseI64NarrowCol); !ok {
		t.Fatalf("round trip lost narrowing: %T", back)
	}
	for i, want := range vals {
		v, ok := back.Get(uint32(i))
		if !ok {
			t.Fatalf("Get(%d) absent after round trip", i)
		}
		if got, _ := v.I64(); got != want {
			t.Fatalf("Get(%d) = %d after round trip, want %d", i, got, want)
		}
	}
}
