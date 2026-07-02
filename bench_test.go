package chickpeas_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// benchGraph is a deterministic pseudo-random graph shared by the
// benchmarks; mirrors the shape of the Rust criterion fixtures. Track
// Go-vs-Go regressions with benchstat over repeated runs; Rust-vs-Go
// comparisons are documentation-only.
func benchGraph(b *testing.B, n uint32, m int) *chickpeas.Snapshot {
	b.Helper()
	builder := chickpeas.NewBuilder(int(n), m)
	for i := range n {
		builder.AddNodeWithID(i, "Node")
	}
	seed := uint64(0xBEEF)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for range m {
		u := uint32(next() % uint64(n))
		v := uint32(next() % uint64(n))
		builder.AddRel(u, v, "KNOWS")
	}
	return builder.Finalize()
}

func BenchmarkFinalize(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		builder := chickpeas.NewBuilder(20_000, 100_000)
		for i := range chickpeas.NodeID(20_000) {
			builder.AddNodeWithID(i, "Node")
			builder.SetProp(i, "score", int64(i))
		}
		for i := range 100_000 {
			builder.AddRel(uint32(i%20_000), uint32((i*7)%20_000), "KNOWS")
		}
		b.StartTimer()
		builder.Finalize()
	}
}

func BenchmarkNeighborsScan(b *testing.B) {
	g := benchGraph(b, 20_000, 100_000)
	m := g.Match("KNOWS")
	b.ResetTimer()
	for b.Loop() {
		total := 0
		for v := range chickpeas.NodeID(20_000) {
			for range g.NeighborsMatch(v, chickpeas.Outgoing, m) {
				total++
			}
		}
		if total != 100_000 {
			b.Fatal("scan lost rels")
		}
	}
}

func BenchmarkBFSDistances(b *testing.B) {
	g := benchGraph(b, 20_000, 100_000)
	m := chickpeas.MatchAll()
	b.ResetTimer()
	for b.Loop() {
		g.BFSDistances(0, chickpeas.Both, m, chickpeas.NoMaxDepth)
	}
}

func BenchmarkPageRank(b *testing.B) {
	g := benchGraph(b, 20_000, 100_000)
	b.ResetTimer()
	for b.Loop() {
		g.PageRank(true, 0.85, 5)
	}
}

func BenchmarkAggregate(b *testing.B) {
	builder := chickpeas.NewBuilder(50_000, 0)
	for i := range chickpeas.NodeID(50_000) {
		builder.AddNodeWithID(i, "Node")
		builder.SetProp(i, "score", int64(i%1000))
	}
	g := builder.Finalize()
	b.ResetTimer()
	for b.Loop() {
		res, err := g.Aggregate("Node").Filter("score", chickpeas.OpGe, 100).Bin("score", 250, 500, 750).Sum("score").Run()
		if err != nil || res.Total != 45_000 {
			b.Fatalf("aggregate: %v %v", res, err)
		}
	}
}

func BenchmarkRCPGRoundTrip(b *testing.B) {
	raw, err := chickpeas.ReadRCPGFile("rcpg/testdata/conformance/big.rcpg")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		section := raw.ToGraphSection()
		_ = section
	}
}
