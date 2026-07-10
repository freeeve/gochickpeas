// Benchmarks the per-candidate label membership probe through a compiled
// NodeMatcher -- the pattern matcher's inner-loop label test.
package graph

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func BenchmarkNodeMatcherLabelProbe(b *testing.B) {
	const n = 1 << 20
	bld := chickpeas.NewBuilder(n, 1)
	for i := 0; i < n; i++ {
		labels := []string{"Sparse"}
		if i%3 != 0 { // ~67%: dense band
			labels = []string{"Dense"}
		}
		if _, err := bld.AddNode(labels...); err != nil {
			b.Fatal(err)
		}
	}
	g := New(bld.Finalize())
	m := g.CompileNodeMatcher([]string{"Dense"}, nil)
	b.ResetTimer()
	hits := 0
	for i := 0; i < b.N; i++ {
		if g.NodeMatcherAccepts(m, chickpeas.NodeID((i*2654435761)&(n-1))) {
			hits++
		}
	}
	_ = hits
}
