# 055 -- go-gql >10x slower than go-native on 11 SF1 cells

Filed from rustychickpeas-ldbc on 2026-07-10 (cross-repo ask).

From the LDBC timings sweep published 2026-07-10. Source of truth is
`rustychickpeas-ldbc:viz/public/data/timings.json`; live at rcptest.evefreeman.com.

## Metric

**`gochickpeas (gql)` / `gochickpeas (go)`** -- the Go GQL query engine against the Go port's own
native kernels. Both sides run the **same canonical query shape on the same graph**, so this is the
Go analogue of `rcp-cypher / rcp-native`: how much the query engine costs over a hand-written
kernel. Both sides report `parity=match` on every cell below.

This is an internal Go-vs-Go ratio. It says nothing about Go vs Rust; `go-native / rust-native` is
a separate view and is not what is measured here.

## The 11 cells (SF1, warm)

| ratio | family | query | go-gql ms | go-native ms |
| ---: | --- | --- | ---: | ---: |
| 143.6x | BI | Q18 | 3737.67 | 26.02 |
| 88.7x | BI | Q12 | 1582.75 | 17.85 |
| 82.5x | IC | IC9 | 3817.77 | 46.25 |
| 75.0x | BI | Q1 | 559.31 | 7.45 |
| 58.3x | IC | IC2 | 348.39 | 5.97 |
| 40.5x | IC | IC10 | 290.80 | 7.18 |
| 23.2x | IC | IC8 | 2.85 | 0.12 |
| 21.1x | BI | Q6 | 868.04 | 41.10 |
| 18.5x | BI | Q11 | 138.83 | 7.50 |
| 17.8x | BI | Q13 | 2.11 | 0.12 |
| 14.3x | BI | Q2 | 124.86 | 8.75 |

At SF10 the FinBench CR1 cell also clears the bar (171.44 ms vs 3.36 ms, ~51x).

## Progress (2026-07-10, round 1)

CPU-profiled IC9+Q18 (the two largest absolute costs): 44% of samples sat
in `sort.symMerge` under `sort.SliceStable` -- stable-sorting ~500k
materialized rows to keep LIMIT 20 -- plus ~15% reflection swap overhead
(`reflectlite.Swapper`). Landed in sortRowsByOrder (shared by the
projection and aggregate finalize paths), commit 796cf58:

- the comparator is now a TOTAL order (unique row-index tiebreak), so a
  plain generic `slices.SortFunc` reproduces stable-sort output with no
  reflection;
- under ORDER BY + LIMIT, a bounded max-heap selects the skip+limit
  survivors (one comparison per rejected row) before the small final
  sort -- the round-4 deferred "bounded top-k" item, at the sort layer.

Within-run: IC9 3818 -> 1854 ms (~2.1x), IC2 348 -> 123 ms (~2.8x).
Q1/Q12/Q18 cross-pair deltas were chased down as machine drift: two
stash-interleaved A/B pairs disagreed with each other by 2x on absolute
numbers, and Q18's CPU profile shows zero sort machinery in its top 40
nodes with the change in place -- its cost is subquery probes, untouched
here. Gate 89/89 MATCH.

Still open: streaming sink-level top-k (reject rows at push time --
avoids materializing the 500k-row arena at all and cuts the residual key
eval), Q18/IC-subquery probe costs, the fixed per-query setup on the
sub-3ms cells (IC8, Q13), and the same-commit go-native re-sweep for the
denominator (emitting alongside this round).

## Progress (2026-07-10, round 2) -- fused prop-vs-const comparison

Profiled BI/Q1 (70.7x, ~1.2k allocs, pure CPU): 58% of samples in ceval
tree dispatch over the filter -- the per-row cost of walking
cBin(cProp, cLit) with Value round-trips and the CmpBool closure, times
~3M rows x 2 scans. Landed cCmpPropConst: comp fuses
<prop> <comparison> <constant> (either operand order, all six ops) into
one node running the hoisted column read and the SHARED value.Compare
with no child dispatch -- semantics identical by construction (same
reader path, same coercions incl. the Int/Temporal epoch-millis case,
same three-valued collapse). Slots analysis reports the fused node's
slot so WHERE pushdown is unaffected.

Machine was under a load-66 hugo build all round, so wall-clock was
unusable (every cross-run comparison swung 2-4x uniformly, including
no-filter queries). Evidence recorded instead via a same-process
interleaved micro-benchmark (fuse_bench_test.go): fused 35.3 ns/op vs
unfused 68.2 ns/op -- 1.9x per filter evaluation, stable across
repeats. Full tests green, gate 89/89 MATCH. Re-measure the Q1/Q2/Q6
cells on a quiet machine next round before updating the ratio table.

Next: Q1's residual (aggregator update per row + .year extraction +
CASE dispatch -- candidates: fused CASE-over-int-thresholds, or the
scan-level batch filter which subsumes all of these), Q12/Q18 subquery
probes, streaming sink-level top-k.

## Progress (2026-07-10, round 2 close-out)

Partial re-measure at load ~5-13 (still not reference-quiet): the
fusion-sensitive cells dropped DESPITE residual load -- Q1 543 -> 451 ms,
Q2 77 -> 71 ms -- while filter-light cells drifted up with load; treat
only the direction as signal, defer table updates to a quiet box.

Nine-query combined CPU profile (r9) scopes the next levers, in
evidence order:

1. Sparse-column position lookups: mapaccess2_fast32 (~5% flat) under
   propReader.read -- the core colPosIndex is a map probed per read; a
   rank/select layout (column_rank.go machinery exists) would make
   sparse random reads branchless. Core-level, fully general.
   DONE (round 3): posIndex is now *rankIndex (presence bitmap + block
   ranks; the sparse pair array is position-sorted so rank == slot), and
   rankBlock dropped 512 -> 64 so a probe is two loads + one masked
   popcount -- which also removed the partial-sum loop from the
   finalize-band rank columns' own Gets. Alternated-binary bench (4x
   each, loaded box): rank median 25.5 vs map 27.4 ns/op -- ~7% on the
   probe, plus 10-20x less index memory and no map buckets for GC to
   scan. Gate 89/89 MATCH.
2. AppendNeighborsMatch inner loop (18% cum, biggest single site):
   RelMatch.matches per rel + roaring binarySearch under the type
   probes -- hoisting type containment to range/bitmap intersection is
   the candidate. Core-level.
3. genMatches / subquery probes for Q18/Q12 (72% cum across the mix).

### Same-commit table (both sides at 77473b8, emitted to bench-out)

The apples-to-apples re-measure the filing asked for; every cell improved
against the mixed-commit table (rounds 6/6a/6b of tasks/028 + this
round's top-k all landed between the two measurements):

| cell | gql ms | native ms | ratio | was |
| --- | ---: | ---: | ---: | ---: |
| BI/Q1 | 543.3 | 7.7 | 70.7x | 75.0x |
| BI/Q12 | 1572.2 | 33.4 | 47.1x | 88.7x |
| BI/Q18 | 1778.3 | 44.4 | 40.1x | 143.6x |
| IC/IC10 | 143.9 | 7.4 | 19.4x | 40.5x |
| BI/Q11 | 135.9 | 8.3 | 16.3x | 18.5x |
| BI/Q6 | 665.4 | 52.6 | 12.7x | 21.1x |
| IC/IC2 | 67.9 | 5.7 | 12.0x | 58.3x |
| IC/IC9 | 519.4 | 64.1 | 8.1x | 82.5x |
| BI/Q2 | 76.6 | 10.3 | 7.5x | 14.3x |
| IC/IC8 | 0.7 | 0.2 | 4.4x | 23.2x |
| BI/Q13 | 2.1 | 0.5 | 4.3x | 17.8x |

Four cells now clear the sub-10x bar (IC9, Q2, IC8, Q13). Q1 at 70.7x
barely moved and is nearly alloc-free (~1.2k allocs, 543ms pure CPU) --
the clearest remaining engine-overhead cell; profile it first next
round, then Q12/Q18 (subquery probes).

## Caveat on the denominator -- please re-measure before trusting the exact ratios

**The two sides were measured at different gochickpeas commits.** `go-gql` is stamped `56ce1a9`;
`go-native` was last measured at `e1ab6f8` / `29284c3` / `4007dda` / `91f8678` depending on the
family. The ldbc harness falls back to the denominator's latest available record when the
numerator's commit has no matching native run (this is what the viz Dashboard does too), so each
ratio above mixes commits.

The ordering is almost certainly right -- these are 14x-to-144x gaps, far beyond any plausible
drift between adjacent Go commits -- but the exact numbers will move. **A go-native sweep at
`56ce1a9`** would make this an apples-to-apples table, and would also fill in the
`go-native / rust-native` view, which currently renders "no data" for the same reason.

Ratios exclude cells with a sub-0.1 ms denominator or sub-1 ms numerator; at that scale the ratio
is a timer artifact rather than a signal (IC IS7 and IS2 read 187x and 83x on 0.04 ms denominators,
and FinBench CR5/CR2/CR6 read 24x-53x on denominators that round to 0.00 ms -- all excluded).

## Suggested reading order

BI Q18 and IC IC9 are the two largest absolute costs (3.7 s and 3.8 s of GQL time against 26 ms and
46 ms of kernel time). BI Q1 at 75x on a 559 ms query is the cheapest place to see the overhead
clearly. IC IC8 and BI Q13 are sub-3 ms in absolute terms -- real ratios, but likely dominated by
fixed per-query engine setup rather than anything in the plan.
