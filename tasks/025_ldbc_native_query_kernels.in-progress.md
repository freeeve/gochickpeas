# 025 — native Go kernels for each LDBC benchmark query (per-query `gochickpeas (go)` column)

> Filed by the rustychickpeas-ldbc session (uncommitted, cross-repo coordination). The kernel
> implementations are Go work and belong here; the parity reference is provided ldbc-side
> (rustychickpeas-ldbc tasks/263). Owner: gochickpeas.

## Goal

Today `cmd/ldbcbench` emits only the **6 core primitives** (`neighbor_groups`, `fold_via`, …) under
family `native`. That's a lone column with no rcp counterpart. Instead, implement **each LDBC benchmark
query as a native Go kernel**, across **all five families** — BI Q1–Q20, IC (IC1–13 + IS1–7 + IC14),
FinBench (CR1–12 + SR1–6), SPB (30 queries), and GA (BFS/PR/WCC/CDLP/LCC/SSSP) — so `gochickpeas (go)`
becomes a real **per-query native column that lands in each family**, directly next to
`rcp-native (rust)` (the rcp rust floor). Then
`go-native % rust-native` is a true per-query comparison, and the go native column mirrors the rcp
native column instead of being 6 unrelated primitives.

This is the native analog of what `cmd/gqlbench` already does for the GQL engine: same graphs, same
per-query reference hashes, same rowhash/v1 — but the rows come from **hand-written Go kernel code**
(no query language) rather than the GQL engine.

## Approach

1. **New runner** `cmd/ldbcnativebench` (or extend `ldbcbench`) that is manifest-driven like
   `gqlbench`: read the ldbc-side **native manifest** (`native_variants.tsv`, tasks/263 there — cols
   `family query variant graph refhash norm`, NO query text since native is code), load each distinct
   graph once, run the matching native Go kernel, normalize + `RowsHash` the rows, and gate on
   `hash == refhash` (a DIFF fails the run, exactly like the two existing benches). Only a MATCH emits.
2. **Kernel registry keyed by (family, query).** Generalize `internal/ldbc/kernels.go`: today `Kernel`
   is `{Name, Rows func(g) ([][]int64,error), Want}`. Add a per-query registry whose `Rows` returns
   **`[][]any`** (LDBC queries project strings/floats/ints, not just int64) so `rowhash.RowsHash`
   ([]any already supported) can hash them against the manifest refhash. The `Want`/fixture path is not
   needed for these — the manifest refhash IS the oracle (the 6 primitive kernels keep their fixture
   path).
3. **Emit** one `ldbc.Record` per MATCH: `Family=<family>`, `Query=<qid>`, `Engine="gochickpeas (go)"`,
   `Shape="native kernel"`, `Variant=<manifest variant>`, warm median over `-runs`, stamped with the
   Go HEAD (`ldbc.HeadStamp`), appended to `bench-out/emitted_gochickpeas.jsonl`. Because Family is now
   BI/IC/FinBench, these rows join those families on the ldbc viz automatically.
4. **Reference implementation to port:** rcp's rust native kernels are the spec —
   `rustychickpeas-ldbc/src/bi/*.rs` (BI), `src/interactive/` (IC), `src/finbench/` (FinBench),
   `src/graphalytics/` (GA). Match their result shape/order (the refs are emitted in official RETURN
   column order); the manifest's `norm` op (round3 / unwrap1 / msday / …) reconciles float precision
   and variable-arity rows, same as the GQL side.

## Phasing (each query flips to emitting the moment it MATCHes)

- **BI first** (20 queries) — start with the ones already implemented as GQL twins so the reference
  rows are known-reachable; note Q10/Q15/Q19/Q20 are weighted/GDS in rcp (native kernels exist, port
  them). Then **IC**, then **FinBench** (CR8 is the stateful claim-order BFS — hardest; can defer),
  then **SPB** (30 queries; fits the rowhash-refhash model — SPB result rows), then **GA**.
- **GA is a separate track** (Eve confirmed all families). Graphalytics parity is NOT rowhash of
  result rows — the 6 algorithms (BFS/PR/WCC/CDLP/LCC/SSSP) produce a per-vertex value over their own
  datasets (wiki-Talk, …), validated against the official `<vertex-id> <value>` reference files
  (float algos PR/LCC need an epsilon), the way `rustychickpeas-ldbc/src/graphalytics/validate.rs`
  does. So GA needs its own graphs + its own validate/emit path, not the `native_variants.tsv`
  refhash gate. Coordinate the GA reference/dataset provisioning with ldbc tasks/263.
- Coverage is incremental by design (mirror `gqlbench`'s "skip loudly" for the not-yet-implemented).

## Notes

- The existing 6 primitives can stay as a separate `native` family (a micro-benchmark set) or be
  retired once the per-query kernels exist — they were the bootstrap, not the destination.
- Parity uses the SAME rowhash/v1 + refhashes as GQL, so a query's native kernel and its GQL twin gate
  against the identical reference — a nice internal check (both must hash-equal the ref).
- Blocked in the manifest (IC14 weighted-SP is fine natively; only truly-native-infeasible rows would
  be marked) — coordinate the blocked set with tasks/263.
