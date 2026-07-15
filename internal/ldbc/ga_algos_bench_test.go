// BFS/PageRank/CDLP benchmarks over a real Graphalytics dataset,
// env-gated like the LCC bench: set GOCHICKPEAS_GA_DATA to the dataset
// dir. Added for the task-068/305 regression check on the wiki-Talk GA
// cells; they double as the same-harness A/B rig for GA kernel work.
package ldbc

import (
	"os"
	"testing"
)

func gaWikiTalk(b *testing.B) *GADataset {
	b.Helper()
	dir := os.Getenv("GOCHICKPEAS_GA_DATA")
	if dir == "" {
		b.Skip("GOCHICKPEAS_GA_DATA not set")
	}
	ds, err := LoadGADataset(dir, "wiki-Talk")
	if err != nil {
		b.Fatal(err)
	}
	return ds
}

func BenchmarkGABFSWikiTalk(b *testing.B) {
	ds := gaWikiTalk(b)
	src := uint32(0)
	if ds.Params.BFSSource != nil {
		src = *ds.Params.BFSSource
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := GABFS(ds.Graph, src, ds.Params.Directed); len(out) == 0 {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkGASSSPWikiTalk(b *testing.B) {
	ds := gaWikiTalk(b)
	src := uint32(0)
	if ds.Params.SSSPSource != nil {
		src = *ds.Params.SSSPSource
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := GASSSP(ds.Graph, src, ds.Params.Directed); len(out) == 0 {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkGAPageRankWikiTalk(b *testing.B) {
	ds := gaWikiTalk(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := GAPageRank(ds.Graph, ds.Params.Directed, ds.Params.PRDamping, ds.Params.PRIterations); len(out) == 0 {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkGACDLPWikiTalk(b *testing.B) {
	ds := gaWikiTalk(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := GACDLPSeeded(ds.Graph, ds.Params.Directed, ds.Params.CDLPIterations, ds.VertexOfNode); len(out) == 0 {
			b.Fatal("empty result")
		}
	}
}
