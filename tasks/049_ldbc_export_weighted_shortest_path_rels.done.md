# 049 — go: materialize the BI weighted-shortest-path rels in the ldbc rcpg export (Q15/Q19/Q20)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** The `COST` clause
(d2ff0b6, 045) works -- but there's **no weighted graph to run it on**. BI Q15/Q19/Q20 traverse
materialized weight relationships that the go ldbc export doesn't produce.

## Symptom
```
MATCH ()-[r:q15weight]->() RETURN count(r) AS c   -- on sf1_canonical.rcpg -> 0
```
`interactsWith` and `cohort` are likewise absent. So `gql/bi/q15.gql`'s
`MATCH p = ANY SHORTEST (a)-[r:q15weight]-{1,}(b) COST r.w` finds no path and returns 0 (DIFF vs ref
`0.715873`). This is a graph/export gap, not a query or COST-clause gap.

## What the rels are (from the rcp side, `python/cypher/bi/weighted.py`)
BI's weighted-SP bucket materializes three derived weight relations on the canonical SF1 graph, each with a
numeric weight property `w` (all positive):
- **q15weight** (Q15) — between Persons
- **interactsWith** (Q19) — Person interaction graph
- **cohort** (Q20) — Person cohort graph

The rcp weighted snapshot builds these; the go `spbexport`/ldbc-export path does not.

## Ask
Add the three weight relations (with property `w`) to the canonical rcpg export (or a sibling weighted
rcpg the gql manifest can point BI Q15/Q19/Q20 at). Once present, the already-authored `ANY SHORTEST … COST
r.w` + `FOR e IN rels(p) RETURN sum(e.w)` queries should compute the min-edge-weight cost. Repro:
`gql/bi/q15.gql`, `q19`, `q20` (each `-- blocked:` on this). Reference weights/derivation:
`rustychickpeas-ldbc/python/cypher/bi/weighted.py`.

## Disposition (gochickpeas session, 2026-07-06): shipped as cmd/weightedexport -- sibling weighted rcpg

```
go run ./cmd/weightedexport \
  -in  ~/rustychickpeas-ldbc/export/sf1_canonical.rcpg \
  -out ~/rustychickpeas-ldbc/export/sf1_weighted.rcpg
```

Already run once: `export/sf1_weighted.rcpg` exists (17,885,308 rels = canonical + q15weight 346,028 +
interactsWith 311,328 + cohort 13,732 + **ic14weight 346,028 as a bonus** for IC14 later). The weights
derive graph-side from the canonical snapshot -- no CSV dependency -- by the exact map builders the
native Q15/Q19/Q20 kernels traverse (extracted to internal/ldbc/weights.go; native gate still 89/89).
Both directions per undirected knows pair, float property `w`, matching python/cypher/weights.py 1:1.

Verified against the refs on sf1_weighted.rcpg with your authored queries verbatim (headers stripped):
- **Q15 -> 0.715873** (ref exact), 0.08s
- **Q19 -> all 6 ref rows exact** (0.118056 ... 0.411111), 0.4s
- **Q20 -> [26388279068667, 11.0]** (ref exact), 0.04s

(Q20 initially took 63s: the COST Dijkstra keyed states on (node, hops) even when unbounded, so the 8
unreachable sources exploded the state space; unbounded searches now key the node alone -- plain
Dijkstra. Same fix benefits Q19.)

Manifest-side (yours): point the Q15/Q19/Q20 rows at `export/sf1_weighted.rcpg` and drop the
`-- blocked:` headers. Regenerating after a canonical re-export is one command (above).
