# 026 — SPB native query kernels (blocked on ldbc-side graph export + manifest rows)

Split out of tasks/025 (all other families shipped at 59/59 parity + GA validated).

## Goal

Port the 30 SPB queries (rustychickpeas-ldbc `src/spb/{q*,a*}.rs`, ~6k lines) as native Go kernels,
gated on per-query rowhash refs, emitting `Family=SPB` rows next to `rcp-native (rust)` — same shape
as the BI/IC/FinBench kernels (tasks/025).

## Blocked on (ldbc-side, their tasks/263)

1. **SPB canonical .rcpg export** — no export exists (`export_gochickpeas.rs` covers SF1 only; the SPB
   graph lives behind their N-Triples loader). The Go runner needs `export/spb_canonical.rcpg` (or
   equivalent) plus the canonical property/rel naming it carries.
2. **Per-query manifest rows** — `python/refs/spb/spb.parity.rust.json` holds all 30 queries' oracle
   rows (kinds: uris / uri_opt / kv), but the native manifest rows (refhash + norm per query) are their
   tasks/263 deliverable; reshape happens in `viz/native_manifest.py`.

## Once unblocked

- Kernels register under `FinBench`-style ids (`SPB/Q1`, `SPB/A17`, …) in `internal/ldbc`; the runner
  (`cmd/ldbcnativebench`) needs no changes beyond the manifest rows.
- Port specs: each `src/spb/<q>.rs` module; params pinned in the parity JSON's `params` block
  (word/topic/entB/category/audience/cwType/date window, geo lat/lon/km).
- Full-text (q8/a20) and geo (q6/a17) queries map onto the core FullTextField / GeoIndex, which the Go
  port already has.
