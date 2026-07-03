// GQL engine benchmarks over a deterministic xorshift graph (the root
// bench_test.go fixture shape). Go-vs-Go regressions tracked with
// benchstat; Rust-vs-Go comparisons are documentation-only, never gated.
package gql

import (
	"strconv"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// gqlBenchGraph is n Person nodes with age properties and m random KNOWS
// rels (deterministic xorshift, mirroring the root benchGraph).
func gqlBenchGraph(b *testing.B, n uint32, m int) *chickpeas.Snapshot {
	b.Helper()
	builder := chickpeas.NewBuilder(int(n), m)
	for i := range n {
		if _, err := builder.AddNodeWithID(i, "Person"); err != nil {
			b.Fatal(err)
		}
		_ = builder.SetProp(i, "age", int64(20+i%60))
		_ = builder.SetProp(i, "name", "p"+strconv.FormatUint(uint64(i), 10))
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
		_, _ = builder.AddRel(u, v, "KNOWS")
	}
	return builder.Finalize("name", "age")
}

const benchQuery = "MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 60 " +
	"RETURN f.name AS name, count(*) AS c ORDER BY c DESC LIMIT 10"

func BenchmarkParse(b *testing.B) {
	for b.Loop() {
		if _, err := parser.Parse(benchQuery); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlan covers parse + desugar + plan (the whole front half).
func BenchmarkPlan(b *testing.B) {
	g := graph.New(gqlBenchGraph(b, 20_000, 100_000))
	b.ResetTimer()
	for b.Loop() {
		q, err := parser.Parse(benchQuery)
		if err != nil {
			b.Fatal(err)
		}
		if err := semantics.Desugar(q); err != nil {
			b.Fatal(err)
		}
		if _, err := plan.Build(q, g); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExecuteScanFilter(b *testing.B) {
	g := gqlBenchGraph(b, 20_000, 100_000)
	q := "MATCH (p:Person) WHERE p.age > 70 RETURN p.name AS name"
	b.ResetTimer()
	for b.Loop() {
		rows, err := Run(g, q)
		if err != nil {
			b.Fatal(err)
		}
		for range rows.All() {
		}
	}
}

func BenchmarkExecuteExpandAgg(b *testing.B) {
	g := gqlBenchGraph(b, 20_000, 100_000)
	b.ResetTimer()
	for b.Loop() {
		rows, err := Run(g, benchQuery)
		if err != nil {
			b.Fatal(err)
		}
		for range rows.All() {
		}
	}
}

func BenchmarkExecuteVarLength(b *testing.B) {
	g := gqlBenchGraph(b, 20_000, 100_000)
	q := "MATCH (p:Person {name: 'p42'})-[:KNOWS]->{1,2}(f) RETURN count(DISTINCT f) AS n"
	b.ResetTimer()
	for b.Loop() {
		rows, err := Run(g, q)
		if err != nil {
			b.Fatal(err)
		}
		for range rows.All() {
		}
	}
}

func BenchmarkPlanCacheHit(b *testing.B) {
	g := gqlBenchGraph(b, 20_000, 100_000)
	cache := NewPlanCache(DefaultCacheBytes)
	q := "MATCH (p:Person) WHERE p.age > 75 RETURN p.name AS name LIMIT 5"
	if _, err := cache.Run(g, q); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		rows, err := cache.Run(g, q)
		if err != nil {
			b.Fatal(err)
		}
		for range rows.All() {
		}
	}
}
