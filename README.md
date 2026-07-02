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
Rust-generated golden files. See [DESIGN.md](DESIGN.md) and `tasks/`.

## Packages

- `chickpeas` (root) -- the engine: `NewBuilder(...)` -> `Finalize()` ->
  `*Snapshot` queries; `ReadRCPGFile`/`WriteRCPGFile` for interchange.
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
