# 024 -- gql xcheck + v0.2.0 (M22)

The port's final milestone: the cross-engine golden-corpus harness (record
schema documented for the rustychickpeas exporter, tasks/200 there; a
Go-generated seed corpus pins the schema and covers every feature family
today), FuzzQuery invariant fuzzing, PARITY-GQL.md rows for the Rust gql
crate's public API, gql benchmarks, DESIGN.md/README updates, and the
v0.2.0 tag (tagged after review).

Gate: corpus seed green under the harness; 20s FuzzQuery smoke clean;
gofmt -s / vet / full -race suite green; gql tree coverage >= 80%.
