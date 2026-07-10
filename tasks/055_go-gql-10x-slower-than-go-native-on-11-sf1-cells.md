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
