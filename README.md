# gochickpeas

Go implementation of [RustyChickpeas](https://github.com/freeeve/rustychickpeas):
a high-performance in-memory graph database using Roaring bitmaps and CSR
adjacency. Reads and writes RCPG graph files byte-compatibly with the Rust
implementation.

**Status:** the engine core is ported (v0.1.0): Builder -> immutable
Snapshot -> Manager, CSR traversal, columnar properties with lazy equality/
full-text/geo indexes, search kernels (BFS family, Dijkstra, bidirectional
weighted shortest path), aggregation kernels (RootsVia/FoldVia/CoOccurring/
CommonNeighborCounts/NeighborGroups + a fluent Aggregation), and the
Graphalytics analytics set (WCC/PageRank/CDLP/LCC/SSSP). The RCPG/RRSR
codecs are conformance-tested byte-for-byte against the Rust codec in both
directions, and Builder-finalized snapshots serialize byte-identically to
Rust-generated golden files. v0.2.0 adds the GQL query engine. See
[DESIGN.md](DESIGN.md) and `tasks/`.

## Packages

- `chickpeas` (root) -- the engine: `NewBuilder(...)` -> `Finalize()` ->
  `*Snapshot` queries; `ReadRCPGFile`/`WriteRCPGFile` for interchange, and
  `ReadNQuads`/`WriteNQuads` for RDF N-Quads/N-Triples import/export
  (deterministic output, rel props via named-graph-per-edge, transparent
  gzip on read, `.gz` path suffix on write).
- `rcpg` -- the RCPG graph-file codec: `Parse`/`Write`, topology-only
  options, and lazy section-planned loading (`ParseLazy`/`SectionFetch`)
  for range-fetched transports.
- `rcpg/rrsr` -- the RRSR record store (roaringrange RECORDS.md-compatible):
  batched range planning over per-node payloads.
- `nodeset` -- the roaring-backed node-id set query results compose
  through.

```go
b := chickpeas.NewBuilder(0, 0)
alice, _ := b.AddNode("Person")
bob, _ := b.AddNode("Person")
b.SetProp(alice, "name", "Alice")
b.AddRel(alice, bob, "KNOWS")
g := b.Finalize()
for n := range g.Neighbors(alice, chickpeas.Outgoing, "KNOWS") {
    _ = g.Prop(n, "name").StrOr("?")
}
```

```go
raw, _ := os.ReadFile("graph.rcpg")
g, err := rcpg.Parse(raw)             // or rcpg.ParseWith(raw, rcpg.TopologyOnlyParseOptions())
if err != nil { ... }
for _, nbr := range g.OutNeighbors(42) { ... }
```

## GQL

`gql` is a read-only query engine over a `*Snapshot`, speaking the ISO GQL
read subset documented in [gql/GRAMMAR.md](gql/GRAMMAR.md) (a port of the
`rustychickpeas-gql` Cypher engine with a GQL surface): pattern matching
with quantified paths and `ANY`/`ALL SHORTEST`, `OPTIONAL MATCH`,
aggregation, `FOR`, `CALL {}` subqueries, `CALL` procedures over the
engine's analytics/full-text/geo kernels, `EXPLAIN`/`PROFILE`, prepared
statements, and a byte-budgeted plan cache.

```go
rows, err := gql.Run(g,
    "MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 30 "+
        "RETURN f.name AS name, count(*) AS c ORDER BY c DESC LIMIT 10")
if err != nil { ... }
for row := range rows.All() {
    name, _ := row.Get("name")
    c, _ := row.Get("c")
    fmt.Println(name, c)
}
```

Query-level parity with the Rust engine is pinned by the golden corpus
under `gql/testdata/xcheck/` (see its README for the record schema and
the Rust exporter lockstep).

## Development

```bash
go test ./... -race          # unit + conformance tests
go test ./rcpg -fuzz=FuzzParse -fuzztime=30s
go run ./rcpg/cmd/gencorpus out/   # write the corpus for the Rust reverse interop test
```

The golden conformance corpus in `rcpg/testdata/conformance/` is generated
by the Rust repo (`cargo run -p rustychickpeas-format --example
gen_conformance <dir>`), which owns the frozen byte-layout spec (FORMAT.md).

## License

MIT
