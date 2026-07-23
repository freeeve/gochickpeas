# gochickpeas

A fast, in-memory graph database for Go: Roaring-bitmap adjacency, CSR
traversal, columnar properties, a suite of graph-analytics kernels, and a
read-only GQL query engine--with no external runtime dependencies.

gochickpeas is a Go port of
[RustyChickpeas](https://github.com/freeeve/rustychickpeas) and reads and
writes the `.rcpg` graph format byte-for-byte compatibly with it, so graphs
move between the two engines with no conversion step.

[![Go Reference](https://pkg.go.dev/badge/github.com/freeeve/gochickpeas.svg)](https://pkg.go.dev/github.com/freeeve/gochickpeas)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Features

- **Graph core**: immutable, columnar snapshots built through a `Builder`;
  CSR adjacency with Roaring-bitmap label and relationship sets; lazily
  built equality, full-text, and geo indexes.
- **Traversal and search**: the BFS family, Dijkstra, and bidirectional
  weighted shortest paths, exposed as Go iterators.
- **Analytics**: the Graphalytics set--WCC, PageRank, CDLP, LCC, and SSSP.
- **Aggregation**: composable co-occurrence, neighbor-grouping, and
  fold/roots primitives behind a fluent `Aggregation` API.
- **GQL query engine**: a read-only engine speaking the ISO GQL read
  subset--pattern matching with quantified and `ANY`/`ALL SHORTEST` paths,
  `OPTIONAL MATCH`, aggregation, `CALL {}` subqueries, `CALL` procedures
  over the analytics/full-text/geo kernels, `EXPLAIN`/`PROFILE`, prepared
  statements, and a byte-budgeted plan cache.
- **Interchange**: the RCPG graph-file codec (byte-compatible with
  RustyChickpeas) and RDF N-Quads/N-Triples import/export with transparent
  gzip.
- **Conformance-tested**: the RCPG/RRSR codecs are checked byte-for-byte
  against the Rust implementation in both directions, and query results are
  pinned to a cross-checked golden corpus.

## Install

```bash
go get github.com/freeeve/gochickpeas
```

Requires Go 1.25 or newer.

## Quick start

### Build and traverse a graph

```go
b := chickpeas.NewBuilder(0, 0)
alice, _ := b.AddNode("Person")
bob, _ := b.AddNode("Person")
b.SetProp(alice, "name", "Alice")
b.AddRel(alice, bob, "KNOWS")
g := b.Finalize()

for n := range g.Neighbors(alice, chickpeas.Outgoing, "KNOWS") {
    fmt.Println(g.Prop(n, "name").StrOr("?"))
}
```

### Query with GQL

```go
rows, err := gql.Run(g,
    "MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 30 "+
        "RETURN f.name AS name, count(*) AS c ORDER BY c DESC LIMIT 10")
if err != nil {
    log.Fatal(err)
}
for row := range rows.All() {
    name, _ := row.Get("name")
    c, _ := row.Get("c")
    fmt.Println(name, c)
}
```

The supported query surface is documented in
[gql/GRAMMAR.md](gql/GRAMMAR.md).

### Read a graph file

```go
raw, _ := os.ReadFile("graph.rcpg")
g, err := rcpg.Parse(raw) // or rcpg.ParseWith(raw, rcpg.TopologyOnlyParseOptions())
if err != nil {
    log.Fatal(err)
}
for _, nbr := range g.OutNeighbors(42) {
    // ...
}
```

## Packages

- **`chickpeas`** (root): the engine. `NewBuilder(...)` ->
  `Finalize()` -> `*Snapshot` queries; `ReadRCPGFile`/`WriteRCPGFile`
  for interchange, and `ReadNQuads`/`WriteNQuads` for RDF import/export
  (deterministic output, relationship properties via named-graph-per-edge,
  transparent gzip on read, `.gz` suffix on write).
- **`gql`**: the read-only GQL query engine over a `*Snapshot`.
- **`rcpg`**: the RCPG graph-file codec--`Parse`/`Write`, topology-only
  options, and lazy section-planned loading (`ParseLazy`/`SectionFetch`)
  for range-fetched transports.
- **`rcpg/rrsr`**: the RRSR record store, with batched range planning over
  per-node payloads.
- **`nodeset`**: the Roaring-backed node-id set that query results compose
  through.

## Documentation

- [DESIGN.md](DESIGN.md)--architecture and design decisions.
- [gql/GRAMMAR.md](gql/GRAMMAR.md)--the supported GQL grammar.

## Development

```bash
go test ./... -race                          # unit + conformance tests
go test ./rcpg -fuzz=FuzzParse -fuzztime=30s # fuzz the codec
```

The golden conformance corpus in `rcpg/testdata/conformance/` is generated
by the Rust repo, which owns the frozen byte-layout specification, and the
Go codec is tested against it in both directions.

## License

MIT--see [LICENSE](LICENSE).
