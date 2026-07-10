// Benchmarks the sparse-column indexed random-read path (posIndex).
package chickpeas_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// BenchmarkAppendNeighborsTyped expands random frontiers over a
// single-type match at ~10% type selectivity -- the hot traversal shape.
func BenchmarkAppendNeighborsTyped(b *testing.B) {
	const n = 1 << 16
	bld := chickpeas.NewBuilder(n, n*16)
	types := make([]string, 10)
	for i := range types {
		types[i] = string(rune('A' + i))
	}
	for i := 0; i < n; i++ {
		if _, err := bld.AddNode("N"); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < n*12; i++ {
		u := chickpeas.NodeID((i * 2654435761) & (n - 1))
		v := chickpeas.NodeID((i * 40503) & (n - 1))
		if _, err := bld.AddRel(u, v, types[i%len(types)]); err != nil {
			b.Fatal(err)
		}
	}
	g := bld.Finalize()
	m := g.Match("C")
	var dst []chickpeas.NodeID
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		node := chickpeas.NodeID((i * 2654435761) & (n - 1))
		dst = g.AppendNeighborsMatch(dst[:0], node, chickpeas.Both, m)
	}
	_ = dst
}

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
