# 028 -- allocation reduction: native kernels to ~zero-alloc, then a GQL pass

Filed 2026-07-03 per Eve: several queries show many allocations in the bench emissions,
GQL especially. Reduce allocations toward zero on the native ops first, then do a GQL
pass. Generalized engine wins are the goal -- do NOT over-fit to the LDBC queries; when a
clear win needs a core API change, make it (core has no external consumers yet), as long
as the surface stays conformant with GQL's standards.

## Method (iterate per round)

1. **Bench with allocs**: `go run ./cmd/ldbcnativebench -manifest bench-out/native_variants.tsv`
   appends per-query alloc profiles (`bench-out/profiles_gochickpeas.jsonl`, Mallocs/TotalAlloc
   deltas over one warm run). The rust floor for comparison is their
   `viz/data/profiles.jsonl` (`rcp-native (rust)` rows, counting-allocator allocs/bytes).
2. **Reduce**: attack the biggest allocators with general engine fixes (iterator reuse,
   scratch buffers on Snapshot readers, avoiding per-row boxing, string interning reads,
   set/map recycling). Track per query below; re-verify parity (89/89 must hold) after
   every change.
3. **Profile hot paths**: once allocs are down, `go test -bench`/pprof the remaining hot
   kernels and resolve CPU hot paths the same way (general wins first).
4. **GQL pass**: repeat 1-3 for `cmd/gqlbench` (plan/exec allocations; the deliberately
   deferred perf work in [[gql-port-progress]] -- streaming top-k/aggregate segments,
   bounded top-k heap -- is in scope if benchmarks point there).

## Per-query tracking

Populate from each profiles emission round (family/query: allocs -> after). Baseline is
the 2026-07-03 emission at 29284c3; fill top offenders first.

(baseline table to be appended after the first emission round)

## Constraints

- Parity gate is sacred: every optimization round ends with `-verify-only` 89/89 MATCH.
- Kernel code must stay readable as the ported reference (it is emitted to the viz as
  the code that ran); prefer engine/core improvements over kernel-local contortions.
- Public gql API (`Run`/`RunWithParams`/`Prepared`/`PlanCache`) and the rowhash encoding
  are integration surfaces -- don't break them.
