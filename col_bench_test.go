// Benchmarks the sparse-column indexed random-read path (posIndex).
package chickpeas_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// BenchmarkSparseColIndexedGet random-reads a ~60%-fill i64 column through
// ColIndexed, the shape of a filter probing a property over a scan.
func BenchmarkSparseColIndexedGet(b *testing.B) {
	const n = 1 << 20
	bld := chickpeas.NewBuilder(n, 1)
	for i := 0; i < n; i++ {
		id, _ := bld.AddNode("N")
		if i%5 != 0 && i%3 != 0 { // ~53% fill: sparse band
			_ = bld.SetProp(id, "v", int64(i))
		}
	}
	g := bld.Finalize()
	col, ok := g.ColIndexed("v")
	if !ok {
		b.Fatal("no column")
	}
	c := col.I64()
	b.ResetTimer()
	var sink int64
	for i := 0; i < b.N; i++ {
		pos := uint32((i * 2654435761) & (n - 1)) // scrambled positions
		if v, ok := c.Get(pos); ok {
			sink += v
		}
	}
	_ = sink
}
