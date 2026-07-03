# 027 — emit native-kernel source + gql EXPLAIN plans + alloc profiles for the rcptest viz

> Filed by the rustychickpeas-ldbc session (uncommitted). Pairs with ldbc `tasks/266`. The ldbc viz
> (`rcptest.evefreeman.com`) wants to show, for the two `gochickpeas` columns, the same "what ran /
> how it planned / how it profiled" artifacts it already shows for the rcp engines. All three are
> Go-side emissions the ldbc `import_gochickpeas.sh` will fold into the site's stores.

## Context

Today the Go bench emits **timings only** (`bench-out/emitted_gochickpeas.jsonl`, engines
`gochickpeas (go)` and `gochickpeas (gql)`, auto-stamped to Go HEAD via `internal/ldbc.HeadStamp`).
The viz already has, for rcp: Cypher text + `EXPLAIN` plan (`plans.jsonl`) and alloc profiles
(`profiles.jsonl`). This task makes `cmd/ldbcbench` / `cmd/gqlbench` also emit the three artifacts
below, in a schema the ldbc side folds directly.

Two enablers already exist here:
- The GQL engine implements `EXPLAIN`/`PROFILE` (`tasks/022`, done) — `gqlbench` just needs to run a
  query in explain mode and capture the plan text.
- Native kernels register via `registerNative(family, query, …)` (`internal/ldbc/native_*.go`), so
  each `(family, query)` maps to a concrete function whose source can be sliced out.

## Deliverable 1 — native kernel source (`gochickpeas (go)`)

For each registered native kernel, emit its Go source so the viz can show the code behind each
`gochickpeas (go)` cell. Suggested: `cmd/ldbcbench` writes `bench-out/code_gochickpeas.jsonl`, one
record per kernel:

    {family, query, engine:"gochickpeas (go)", lang:"go", source, srcRef, engineCommit, engineDate}

- `source` = the kernel function body text; `srcRef` = `file:line` (e.g.
  `internal/ldbc/native_bi_c.go:31`). Commit-stamp with the same `HeadStamp` the timings use.
- Getting the function text: `go:embed` the `internal/ldbc/native_*.go` files and slice by
  `go/parser` func positions, or a small generator that maps the `registerNative` table to source
  spans. Whatever keeps `srcRef` accurate against the swept commit.

## Deliverable 2 — gql EXPLAIN plans (`gochickpeas (gql)`)

In `cmd/gqlbench`, for each manifest query, run the engine's `EXPLAIN` (per `tasks/022`) and emit the
plan text. Suggested `bench-out/plans_gochickpeas.jsonl`:

    {family, query, variant, engine:"gochickpeas (gql)", cypher, plan, engineCommit, engineDate}

- `cypher` = the manifest query text; `variant` = the manifest variant. Plan text format is yours —
  the viz renders it as a monospace block, so a readable multi-line `EXPLAIN` dump is ideal.

## Deliverable 3 — allocation / row profiles

Emit a per-query profile for both passes. Suggested `bench-out/profiles_gochickpeas.jsonl`:

    {family, query, engine:"gochickpeas (go)"|"gochickpeas (gql)", allocs, bytes, rows, engineCommit, engineDate}

- Go allocs aren't the same currency as the Rust `alloc`-count instrumentation the rcp profiles use.
  Prefer `runtime.MemStats` `Mallocs`/`TotalAlloc` deltas around the timed region, or
  `testing.AllocsPerRun`-style measurement. Emit `bytes` + `allocs` if clean; always emit `rows`.
  The ldbc side will label the Go column so it isn't mistaken for the Rust alloc counter — just be
  explicit in the record about what the numbers mean.

## Schema handshake with ldbc

The three files above land in `bench-out/` alongside `emitted_gochickpeas.jsonl`; ldbc
`import_gochickpeas.sh` folds them into `plans.jsonl` / `profiles.jsonl` / `code.jsonl` (dedupe by
`(family, query, engine, engineCommit)`). Keep field names as listed so the fold is a straight merge;
if you diverge, note it here and the ldbc side (`tasks/266`) adapts. No changes to the existing
timings emission — this is additive.

## Status
Open (filed by ldbc session, uncommitted). Enablers done (`022` explain/profile, `025` native
kernels). Pairs with ldbc `tasks/266`.

## 2026-07-03 update (gochickpeas session) -- DONE

All three deliverables shipped, scoped per Eve to the aligned kernel set (BI/IC/FinBench via the
`registerNative` registry -- SPB joins automatically when `026` lands -- plus GA); the six bootstrap
core primitives are excluded, see the removal note below. Field names are exactly as specified
above; the only addition is a `measure` string on every profile record.

- **Deliverable 1** -- `internal/ldbc/source.go`: `go:embed`s `native_*.go` + `ga_algos.go` and
  slices each kernel function (doc comment included) by `go/parser` positions, so `source`/`srcRef`
  are exactly what the swept commit compiled. `NativeKernelSources()` maps the `registerNative`
  table and cross-checks the live registry (a registered kernel without source fails the run);
  `GAKernelSources()` maps the six Graphalytics algorithms. `cmd/ldbcnativebench` and `cmd/gabench`
  append per-MATCH `bench-out/code_gochickpeas.jsonl` records alongside their timings.
- **Deliverable 2** -- `cmd/gqlbench` runs `gql.Explain` per MATCHed manifest row and appends
  `bench-out/plans_gochickpeas.jsonl` (`cypher` = manifest text, `plan` = multi-line EXPLAIN dump
  with planning time, per-operator tree, and `est` cardinality column).
- **Deliverable 3** -- `bench-out/profiles_gochickpeas.jsonl` from all three emitters
  (`gochickpeas (go)` from ldbcnativebench + gabench, `gochickpeas (gql)` from gqlbench):
  `allocs`/`bytes` are `runtime.MemStats` `Mallocs`/`TotalAlloc` deltas over one warm run after
  `runtime.GC()`; `rows` always present. Each record carries
  `measure: "go runtime.MemStats delta over one warm run: allocs=Mallocs, bytes=TotalAlloc"` so the
  viz can label the Go column as Go heap counters, not the Rust alloc-counter currency
  (settles ldbc `266` open question 3).

Emission gating matches timings: artifacts emit only for rows that MATCHed (GA: rows that emitted a
timing, including reference-less unvalidated ones), never under `-verify-only`; all three files are
append-only like `emitted_gochickpeas.jsonl` -- dedupe by `(family, query, engine, engineCommit)`
on the fold as agreed.

**Removed `cmd/ldbcbench`** (per Eve): the six bootstrap core primitives (family `native`) were
pre-025 scaffolding not aligned with the benchmark kernel set. Their coverage already lives in
`ldbc_test.go` (`TestLDBCStructural`/`TestLDBCKernels` against `testdata/ldbc/sf1_expected.json`),
so only the emitter was deleted -- family `native` rows in `emitted_gochickpeas.jsonl` get no new
emissions and the ldbc side can drop those cells from the viz.
