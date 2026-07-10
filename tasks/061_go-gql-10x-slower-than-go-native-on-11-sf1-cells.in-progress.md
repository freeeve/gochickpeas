
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

## Round-1 scoping (2026-07-10, profile at ddd0c09)

Combined Q8+Q17 CPU profile (2/2 MATCH): 89.7% cumulative under
genMatches with NO single hot site -- levelCandidates 34%, compiled
per-level filter eval (ceval+cevalBin) ~30/24%, NodeMatcherAccepts 7.5%
(roaring container binarySearch 3.9% under it), semijoin
slices.BinarySearch 3.4%, memmove/memclr ~7%. Death-by-interpretation
across the join DFS, not a fixable hotspot: the prior rounds' levers
(folding, fusion, typed adjacency, batch seams) are already in.

Levers, in expected-value order:

1. **Level-batch filter evaluation** -- the repeatedly deferred design:
   evaluate a level's compiled conjuncts column-at-a-time over the
   candidate buffer (the buffer already exists post-AppendNeighbors)
   instead of tree-walking per candidate. Slots analysis + the fused
   cCmpPropConst node are the building blocks; a typed columnar kernel
   for prop-vs-const conjuncts over a candidate slice would displace
   most of the ceval share and much of levelCandidates. Big design;
   needs its own session-fresh implementation pass.
2. **Dense-label bitset representation** -- labels covering a large
   fraction of the id space (Message, Person at SF1) pay a roaring
   container binary search per NodeMatcherAccepts probe; a plain bitset
   Contains for above-threshold labels is a label-kind-keyed
   representation choice (core, general), worth ~5-8% of this mix.
3. **BI Q17 cross-engine comparison** (the rcp note): before engine
   work, diff the Go plan against the native kernel's join order --
   2096x on 2.41 ms native suggests a plan-shape gap (join order /
   anti-join placement), which is planner territory, not eval speed.

Not started in this firing (deliberate): the batch-filter design wants
a fresh implementation session; this round's write-up is the handoff.

## Round 2 (2026-07-10) -- lever 2 landed: dense-label word bitmaps

Snapshot.LabelDense lazily builds a plain word bitmap for labels
covering >= idspace/8; CompileNodeMatcher resolves it once per operator
and the per-candidate label test becomes one load and mask.
Alternated-binary bench: 16 -> 7 ns/probe (2.3x), TestLabelDenseMatchesSet
pins bitmap/set agreement + the sparse-label nil. Gate 89/89 MATCH.

End-to-end on the heavy cells: neutral (Q8 -4%, Q17/Q10/Q4 within
noise at load 11-28) -- consistent with the profile's 4-8% share; their
cost is levers 1 and 3. LabelDense is new public core API -> v0.12.0.

Levers 1 (level-batch filters) and 3 (Q17 plan-shape diff vs the native
kernel, likely shared root with rcp's Q17 pathology) remain the round-3
menu -- still recommended for a fresh session.
