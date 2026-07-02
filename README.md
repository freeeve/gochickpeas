# gochickpeas

Go implementation of [RustyChickpeas](https://github.com/freeeve/rustychickpeas):
a high-performance in-memory graph database using Roaring bitmaps and CSR
adjacency. Reads and writes RCPG graph files byte-compatibly with the Rust
implementation.

**Status:** early. The RCPG/RRSR codecs are complete and conformance-tested
byte-for-byte against the Rust codec (both directions). The engine (builder,
snapshot, query kernels) is being ported milestone by milestone -- see
[DESIGN.md](DESIGN.md) and `tasks/`.

## Packages

- `rcpg` -- the RCPG graph-file codec: `Parse`/`Write`, topology-only
  options, and lazy section-planned loading (`ParseLazy`/`SectionFetch`)
  for range-fetched transports.
- `rcpg/rrsr` -- the RRSR record store (roaringrange RECORDS.md-compatible):
  batched range planning over per-node payloads.

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
