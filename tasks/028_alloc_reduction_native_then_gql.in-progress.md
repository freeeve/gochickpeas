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

### Baseline (emission at 29284c3, 2026-07-03) -- top 30 by allocs

| query | allocs | bytes | rows |
|---|---:|---:|---:|
| BI/Q15 | 34,694,274 | 1,012,225,064 | 1 |
| IC/IC5 | 12,864,253 | 395,305,256 | 20 |
| IC/IC3 | 10,655,771 | 331,541,520 | 20 |
| BI/Q2 | 6,078,097 | 208,545,840 | 100 |
| BI/Q6 | 5,760,484 | 150,232,544 | 100 |
| IC/IC6 | 3,641,088 | 144,453,608 | 10 |
| IC/IC12 | 3,630,682 | 137,337,840 | 20 |
| SPB/a13 | 1,311,207 | 84,551,928 | 336,315 |
| SPB/a14 | 1,259,844 | 39,673,592 | 23,192 |
| BI/Q4 | 547,811 | 20,915,800 | 100 |
| SPB/a5 | 519,710 | 21,190,104 | 108,476 |
| BI/Q9 | 490,543 | 12,295,600 | 100 |
| BI/Q14 | 423,001 | 12,911,776 | 7 |
| SPB/a7 | 331,598 | 11,716,960 | 33,561 |
| IC/IC10 | 284,055 | 32,385,056 | 10 |
| BI/Q3 | 247,030 | 6,634,224 | 20 |
| IC/IC4 | 229,196 | 6,705,536 | 10 |
| SPB/a25 | 228,120 | 19,931,280 | 47,499 |
| BI/Q18 | 200,770 | 21,631,320 | 20 |
| BI/Q12 | 189,790 | 18,034,896 | 86 |
| BI/Q10 | 169,497 | 30,207,248 | 100 |
| SPB/a19 | 156,073 | 5,969,408 | 11,434 |
| SPB/a10 | 151,176 | 7,494,936 | 16 |
| SPB/a9 | 151,115 | 4,080,200 | 1 |
| SPB/q5 | 118,504 | 4,550,912 | 7,898 |
| BI/Q17 | 114,085 | 4,673,400 | 10 |
| SPB/q9 | 113,706 | 4,462,720 | 9,462 |
| BI/Q7 | 107,870 | 3,912,072 | 100 |
| SPB/q7 | 75,486 | 2,452,880 | 4,641 |
| SPB/a6 | 67,097 | 2,028,128 | 3 |

Suite total: 85,342,501 allocs. Rust floor: BI ~0, IC 45-61k, IC12 ~1k.

### Round 1+2 (traversal accessors e1ab6f8^, Set.Iter e1ab6f8) -- emitted at e1ab6f8

| query | allocs 29284c3 | allocs e1ab6f8 | ms before | ms after |
|---|---:|---:|---:|---:|
| BI/Q15 | 34,694,274 | 18,729 | 114.24 | 35.92 |
| IC/IC5 | 12,864,253 | 2,012 | 692.82 | 513.68 |
| IC/IC3 | 10,655,771 | 249 | 350.86 | 205.60 |
| BI/Q2 | 6,078,097 | 5,409 | 20.47 | 9.32 |
| BI/Q6 | 5,760,484 | 2,746 | 142.50 | 41.10 |
| IC/IC6 | 3,641,088 | 3,669 | 110.79 | 45.19 |
| IC/IC12 | 3,630,682 | 1,375 | 99.37 | 48.54 |
| SPB/a13 | 1,311,207 | 1,009,015 | 62.36 | 56.79 |
| SPB/a14 | 1,259,844 | 46,413 | 36.24 | 18.74 |
| BI/Q4 | 547,811 | 11,132 | 78.04 | 71.73 |
| SPB/a5 | 519,710 | 217,505 | 64.07 | 53.13 |
| BI/Q9 | 490,543 | 42,820 | 17.04 | 12.13 |
| BI/Q14 | 423,001 | 2,943 | 13.85 | 7.49 |
| SPB/a7 | 331,598 | 67,401 | 21.29 | 15.90 |
| IC/IC10 | 284,055 | 1,823 | 10.36 | 7.18 |

Suite: 85,342,501 -> 2,267,808 allocs (97.3% reduction); summed warm medians 2156ms -> 1465ms.
Remaining allocs are dominated by result-row [][]any boxing (per-row, matches the rust side's own per-row materialization) and kernel-local maps.

### Round 3 (gql pass, commit 97cdc78) -- batch Append* on the Graph seam

Root cause found for the gql side: the executor traverses through the graph.Graph
INTERFACE, and an interface-returned iter.Seq can never devirtualize -- every expand /
var-expand hop paid heap closures per row. Fix: seam-conformant batch methods
(AppendNeighborsMatched / AppendNeighborsByType / AppendRelationships) filling
caller-pooled buffers; expandCandidates filters the appended tail in place, varReach and
varWalk.dfs reuse per-walk buffers.

| gql query | allocs eb74933 | after core fixes (669e756) | after batch (97cdc78) | ms 669e756 | ms after |
|---|---:|---:|---:|---:|---:|
| BI/Q6 | 58,432,686 | 40,293,667 | 22,097,892 | 1387 | 1094 |
| BI/Q18 | 29,929,298 | 28,586,884 | 28,517,896 | 4152 | (unchanged) |
| BI/Q12 | 21,211,214 | 21,211,213 | 3,268,572 | 1979 | 1565 |
| IC/IC5 | 14,115,179 | 14,074,932 | 3,425,714 | 3019 | ~3094 |
| IC/IC3 | 13,182,798 | 13,152,743 | 4,687,981 | 782 | 666 |
| IC/IC9 | 10,311,444 | 10,310,594 | 10,275,687 | 5186 | (unchanged) |
| IC/IC6 | 3,841,509 | 3,840,657 | 155,648 | 133 | 81 |
| IC/IC12 | 3,256,988 | 3,256,747 | 436,160 | 126 | 74 |

### Next round (open)

- gql Q18 / IC9 / CR1: allocations sit in eval/aggregate/projection (28.5M / 10.3M /
  4.9M) -- profile the segment pipeline (per-row []value.Value copies in rowsrc/project,
  aggregate group state, value boxing in date/list functions), then the deferred
  streaming top-k/aggregate segments from [[gql-port-progress]] if profiles point there.
- native: remaining ~2.3M suite allocs are result-row [][]any boxing (proportional to
  emitted rows, same materialization cost the rust side pays) and kernel-local maps
  (BI/Q18 199k, SPB a13 1.0M for 336k rows). Only worth touching with a general
  mechanism (e.g. arena rows), not per-kernel tweaks.
- shortest.go filteredNeighbors still uses the iter.Seq seam (low volume; convert if a
  profile ever surfaces it).
