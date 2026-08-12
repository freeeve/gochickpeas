package chickpeas

import (
	"testing"
)

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
