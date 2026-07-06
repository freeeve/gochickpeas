# 045 — go-gql: expose a weighted shortest-path syntax (blocks BI Q15, Q19, Q20)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** The engine has the
machinery but no grammar for it.

## Symptom
BI Q15/Q19/Q20 are min-edge-weight shortest paths, e.g.:
```
MATCH (a:Person {id:14}), (b:Person {id:16}) RETURN cost(shortestPath((a)-[:q15weight*]-(b)), 'w') AS dist
```
go-gql's parser recognizes `weightedshortestpath` only to **reject** it:
```
"…is not GQL: write MATCH p = ANY SHORTEST / ALL SHORTEST <pattern>"
```
but `ANY/ALL SHORTEST` is by **hop count**, not edge weight -- so it can't express these.

## The machinery already exists
`gql/*.go` has an internal eval function
`weightedShortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, w *pathWeight) *nodesRels`
with a `pathWeight` param, and the shortest-path AST carries a `Weight` field (`sp.Weight`). It just isn't
reachable from the query language -- no grammar sets `Weight`.

## Ask
Add a surface syntax that drives `weightedShortestPath` with a rel-weight property. A GQL-flavored spelling,
e.g. `MATCH p = ANY CHEAPEST (a)-[:rel]->+(b) COST rel.w` (or a `WEIGHT <expr>` / `MINIMIZE sum(rel.w)`
clause on the shortest-path pattern) -- whatever fits the parser -- that binds the AST `Weight` to the
existing eval function. Unblocks Q15/Q19/Q20. Those `.gql` files carry a `-- blocked:` header pointing here
with the intended form.

## Disposition (gochickpeas session, 2026-07-06): shipped as a COST clause on ANY SHORTEST

```
MATCH (a:Person {id:14}), (b:Person {id:16})
MATCH p = ANY SHORTEST (a)-[r:q15weight]-{1,}(b) COST r.w
RETURN <path aggregate>
```

- `COST r.w` (the pattern's rel variable) reads the property per edge -- narrowed to the engine's fast
  relationship-weight reader; a numeric literal is a constant per-edge weight; any other expression is a
  per-edge formula over the rel variable. An edge whose weight is non-numeric, negative, or non-finite is
  excluded from the search.
- Undirected (`-{1,}-`) and directed forms work; a `{m,n}` quantifier's max is the hop cap.
- The total path cost isn't a scalar return value (there is no `cost(...)` expression); if Q15/Q19/Q20
  need the distance itself, reduce over the bound path's rels, e.g.
  `LET dist = reduce(acc = 0.0, x IN rels(p) | acc + x.w)` -- wait, `reduce` is not in the GQL subset;
  use a projection over `rels(p)` (`FOR x IN rels(p) ... RETURN sum(x.w)`) or ask here and a
  `cost`-style accessor can be added.
- `ALL SHORTEST` + COST is a plan error (single cheapest path only). Grammar + semantics documented in
  gql/GRAMMAR.md (MATCH section). See also task 042's engine disposition.
