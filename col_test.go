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

// TestColColumnAndF64Slice covers Col.Column (the raw generic reader passthrough)
// and F64Col.Slice (the dense value slice, declined for a sparse column).
func TestColColumnAndF64Slice(t *testing.T) {
	b := NewBuilder(8, 0)
	for i := 0; i < 8; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	// Full presence stores dense; partial presence stores sparse.
	for i := 0; i < 8; i++ {
		if err := b.SetProp(NodeID(i), "score", float64(i)+0.5); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{2, 5} {
		if err := b.SetProp(NodeID(i), "sf", float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	sc, _ := g.Col("score")
	// Column() hands back the underlying reader, preserving the dtype.
	if sc.Column().Dtype() != DtypeF64 {
		t.Fatalf("Column().Dtype() = %v, want DtypeF64", sc.Column().Dtype())
	}
	// A dense f64 column exposes its value slice directly.
	vals, ok := sc.F64().Slice()
	if !ok || len(vals) != 8 {
		t.Fatalf("dense F64 Slice = (len %d, %v), want (8, true)", len(vals), ok)
	}
	for i := range vals {
		if vals[i] != float64(i)+0.5 {
			t.Fatalf("Slice[%d] = %v, want %v", i, vals[i], float64(i)+0.5)
		}
	}
	// A sparse column has no dense slice.
	sf, _ := g.Col("sf")
	if _, ok := sf.F64().Slice(); ok {
		t.Fatal("sparse F64 column must decline Slice")
	}
}

// benchI64Cols builds one dense and one u48-narrow column over the same
// values, read through the typed I64Col fast path.
func benchI64Cols(n int) (dense, narrow I64Col) {
	vals := make([]int64, n)
	for i := range vals {
		// Epoch-millis-like values spanning more than 32 bits so the
		// chooser picks the u48 class.
		vals[i] = 1_263_065_046_975 + int64(i)*7_919
	}
	nc := narrowI64Column(vals)
	if _, isNarrow := nc.(denseI64NarrowCol); !isNarrow {
		panic("fixture did not narrow to a byte class")
	}
	return Col{col: denseI64Col(vals)}.I64(), Col{col: nc}.I64()
}

// BenchmarkI64ColGetDense is the plain dense read: the baseline every
// narrow class is priced against (task 300).
func BenchmarkI64ColGetDense(b *testing.B) {
	dense, _ := benchI64Cols(1 << 16)
	b.ReportAllocs()
	var sink int64
	for i := 0; b.Loop(); i++ {
		v, _ := dense.Get(uint32(i & (1<<16 - 1)))
		sink += v
	}
	_ = sink
}

// BenchmarkI64ColGetNarrowU48 is the same read through the u48 narrow
// class -- the per-read tax the storage narrowing charges a kernel loop.
func BenchmarkI64ColGetNarrowU48(b *testing.B) {
	_, narrow := benchI64Cols(1 << 16)
	b.ReportAllocs()
	var sink int64
	for i := 0; b.Loop(); i++ {
		v, _ := narrow.Get(uint32(i & (1<<16 - 1)))
		sink += v
	}
	_ = sink
}

// TestI64ColGetMany pins the bulk read against per-id Get on every
// storage class (dense, each narrow width, bit, sparse fallback),
// including out-of-range ids.
func TestI64ColGetMany(t *testing.T) {
	build := func(vals []int64) I64Col { return Col{col: narrowI64Column(vals)}.I64() }
	spans := map[string][]int64{
		"u8":  {5, 200, 5},
		"u16": {1000, 60000, 1000},
		"u32": {0, 1 << 30, 7},
		"u48": {1_263_065_046_975, 1_263_065_046_975 + (1 << 40), 1_263_065_046_975},
		"bit": {7, 8, 7},
	}
	for name, seed := range spans {
		t.Run(name, func(t *testing.T) {
			vals := make([]int64, narrowI64MinLen+3)
			for i := range vals {
				vals[i] = seed[i%len(seed)]
			}
			col := build(vals)
			ids := []uint32{0, 1, uint32(len(vals) - 1), uint32(len(vals)), 999999, 2}
			got := make([]int64, len(ids))
			ok := make([]bool, len(ids))
			col.GetMany(ids, got, ok)
			for i, id := range ids {
				wv, wok := col.Get(id)
				if ok[i] != wok || (wok && got[i] != wv) {
					t.Fatalf("%s id %d: GetMany = (%d,%v), Get = (%d,%v)", name, id, got[i], ok[i], wv, wok)
				}
			}
		})
	}
	// Dense plain column (below the narrowing threshold).
	dense := Col{col: denseI64Col([]int64{1, 2, 3})}.I64()
	got := make([]int64, 2)
	ok := make([]bool, 2)
	dense.GetMany([]uint32{2, 5}, got, ok)
	if !ok[0] || got[0] != 3 || ok[1] {
		t.Fatalf("dense GetMany = %v %v", got, ok)
	}
}
