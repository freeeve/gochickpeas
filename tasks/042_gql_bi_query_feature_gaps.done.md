# 042 — go-gql: feature gaps blocking the last BI queries (CORRECTED)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06; corrected after building the
faithful pure-GQL baseline.** An earlier version of this task claimed `collect`/`UNWIND` were unsupported --
**that was wrong** (I'd grepped the wrong path). Re-probed empirically against `sf1_canonical.rcpg`.

## Confirmed SUPPORTED (so more BI queries are authorable than first thought)
`RETURN..NEXT`, `LET`, `FILTER`, `EXISTS{}`, `COUNT{}`, `collect`, `UNION`, `FOR x IN <list>` (GQL's UNWIND),
list concat `[a]+[b]`, `duration({hours:4})` + datetime arithmetic, `substring`, `count(DISTINCT)`,
`ANY/ALL SHORTEST`, var-length `-[:R]->{0,}`. This unblocked **BI Q9, Q10, Q17** (now MATCH) and makes **Q8**
authorable via a rewrite.

## Confirmed GAPS (the real blockers)

1. **`CALL { … }` subquery — unsupported** (parse `error`). Blocks **BI Q4** (its `CALL { … UNION ALL … }`
   that is then `sum()`-ed). Needs `CALL{}` support, or Q4 is hard to express without it.
2. **Pattern-comprehension `size([(a)-[:R]-(b) | x])` — unsupported** (parse `error`). Affects **BI Q8** as
   written. NOTE it is **rewritable** as `COUNT { MATCH (a)-[:R]-(b) }` (which go-gql supports), so Q8 is
   authorable with that rewrite -- a convenience gap, not a hard blocker. Filing since pattern
   comprehensions are common in LDBC.
3. **Weighted shortest path — no grammar.** The parser recognizes `weightedshortestpath` only to REJECT it
   (`"…is not GQL: write MATCH p = ANY SHORTEST / ALL SHORTEST"`), and `ANY/ALL SHORTEST` is by hop-count,
   not edge-weight. But the engine HAS an internal `weightedShortestPath(ctx, a, b, sp, rm, hop, pw)` eval
   function (`gql/*.go`) taking a `pathWeight` -- it just isn't reachable from the query language. Blocks
   **BI Q15, Q19, Q20** (`cost(shortestPath((a)-[:r*]-(b)), 'w')` = min-edge-weight path). Ask: expose a
   weighted shortest-path syntax that drives the existing internal function.

## Net BI GQL disposition (rustychickpeas-ldbc gql/bi/)
MATCH now: **Q9, Q10, Q17** (+ the original 12). Authorable next: **Q8** (COUNT{} rewrite). Blocked here:
**Q4** (CALL{}), **Q15/Q19/Q20** (weighted-shortest grammar) -- these are authored in faithful GQL with a
`-- blocked:` header pointing at this task, so they run the moment the feature lands.

## Engine disposition (gochickpeas session, 2026-07-06)

1. **`CALL { ... }` -- already supported.** It landed with the streaming-executor commit (56ce1a9), likely
   after this task's probe. Both the uncorrelated `CALL { ... UNION ALL ... }` and the correlated GQL
   variable-scope form `CALL (p) { ... UNION ALL ... }` parse, plan, and execute (verified empirically
   through the public API). NOTE for Q4: imports use the GQL scope clause `CALL (p) { ... }`, NOT Cypher's
   `CALL { WITH p ... }` (that spelling gets a targeted parse error pointing at the GQL forms).
2. **Pattern comprehensions -- stay rejected by design** (no ISO GQL spelling; see GRAMMAR.md "Excluded
   surface"). The targeted error now spells out the rewrite (`COUNT { MATCH pat }` / `collect(...)`) and
   fires for every presentation of the form, including patterns that cannot parse as expressions.
3. **Weighted shortest path -- new `COST` clause.** `MATCH p = ANY SHORTEST (a)-[r:T]->{1,}(b) COST <expr>`
   drives the existing Dijkstra kernel. `COST r.w` reads the rel property per edge (narrowed to the fast
   property reader), a numeric literal is a constant weight, anything else is a per-edge formula over the
   rel variable. Non-numeric/negative/non-finite edge weights exclude the edge; `ALL SHORTEST` + COST is a
   plan error. For Q15/Q19/Q20: `cost(shortestPath((a)-[:r*]-(b)), 'w')` becomes
   `MATCH p = ANY SHORTEST (a)-[r:T]-{1,}(b) COST r.w` (undirected works; hop caps honored).
