
## Update 2026-07-10 -- refreshed at same-commit `dc7de70` (perf-watch loop)

The earlier table paired go-gql and go-native at different commits (fallback). The Go session has
since swept newer commits, and go-gql + go-native are now BOTH at **`dc7de70`** -- so these are
clean same-commit ratios, no fallback caveat. Single `canonical` variant per side (no shape
multiplicity). All MATCH.

| ratio | fam | query | go-gql ms | go-native ms |
| ---: | --- | --- | ---: | ---: |
| 2095.8x | BI | Q17 | 5046.61 | 2.41 |
| 763.6x | BI | Q8 | 342.11 | 0.45 |
| 178.6x | BI | Q10 | 4047.38 | 22.66 |
| 46.6x | BI | Q1 | 359.32 | 7.71 |
| 44.8x | BI | Q4 | 3393.66 | 75.82 |
| 44.4x | BI | Q12 | 842.07 | 18.95 |
| 42.2x | BI | Q18 | 1228.08 | 29.12 |
| 19.2x | BI | Q19 | 176.16 | 9.18 |
| 16.3x | BI | Q13 | 1.79 | 0.11 |
| 12.6x | IC | IC2 | 58.68 | 4.66 |
| 11.2x | IC | IC10 | 81.03 | 7.25 |
| 11.1x | BI | Q6 | 662.58 | 59.55 |
| 10.8x | IC | IC9 | 464.82 | 43.22 |
| 10.0x | BI | Q11 | 94.22 | 9.40 |

Leads by absolute GQL cost: **BI Q17 (5.0 s)**, **BI Q10 (4.0 s)**, **BI Q4 (3.4 s)** -- all GQL
seconds against native tens-of-ms. BI Q8 at 764x on a 342 ms query is the cleanest small case to
profile the planner/executor overhead. The list grew from 11 -> 14 cells vs the original filing
(Q19, Q6, IC9 crossed 10x at this commit).

Note (from the rustychickpeas side, same loop): the rcp cypher engine shows an analogous BI Q17
pathology (its shipped Q17 is ~9 s vs a 6 ms native floor), so if the Go GQL planner shares lowering
strategy with rcp, BI Q17 may have a common root worth comparing across the two engines.
