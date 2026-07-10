// LCC benchmark over a real Graphalytics dataset, env-gated like the SF1
// validation harness: set GOCHICKPEAS_GA_DATA to the dataset dir.
package ldbc

import (
	"os"
	"testing"
)

func BenchmarkGALCCWikiTalk(b *testing.B) {
	dir := os.Getenv("GOCHICKPEAS_GA_DATA")
	if dir == "" {
		b.Skip("GOCHICKPEAS_GA_DATA not set")
	}
	ds, err := LoadGADataset(dir, "wiki-Talk")
	if err != nil {
		b.Fatal(err)
	}
	g := ds.Graph
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := GALCC(g, ds.Params.Directed); len(out) == 0 {
			b.Fatal("empty result")
		}
	}
}
