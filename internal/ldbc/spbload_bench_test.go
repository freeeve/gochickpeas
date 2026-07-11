// SPB N-Triples load benchmark, env-gated like the kernel benches: set
// GOCHICKPEAS_SPB_NT to the .nt/.nq path. Drives profiling of the NT
// parse (libcodex/rdf) + property-graph mapping split.
package ldbc

import (
	"os"
	"testing"
)

func BenchmarkLoadSPBNT(b *testing.B) {
	path := os.Getenv("GOCHICKPEAS_SPB_NT")
	if path == "" {
		b.Skip("GOCHICKPEAS_SPB_NT not set")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, _, err := LoadSPBFile(path)
		if err != nil {
			b.Fatal(err)
		}
		if g.NodeCount() == 0 {
			b.Fatal("empty graph")
		}
	}
}
