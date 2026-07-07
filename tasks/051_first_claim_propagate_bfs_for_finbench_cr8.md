# 051 -- core+gql: first-claim value-propagating BFS (unblocks FinBench CR8)

**Scoped 2026-07-07 from the ldbc side's pure-GQL baseline (their tasks/295); awaiting Eve's
sign-off before implementation.** Last remaining engine blocker in the 89-query baseline
(their `gql/finbench/cr8.gql` carries the `-- blocked:` header; the rcp engine gates CR8 on the
same missing primitive -- their tasks/075 scoped it as "first-claim monotonic BFS, needs
sign-off" and shipped everything else, so no cross-engine spelling exists yet to align with.
Whatever we land becomes the precedent).

## The semantics that pattern matching can't express (from the native kernel, native_fin_b.go finCR8)

CR8: trace loan funds from a loan's deposited accounts through transfer/withdraw, <=3 hops,
reporting per reached account [id, inflow/loanAmount, minDistance].

- **Per-seed first-claim BFS.** One BFS per deposit account (seed value = deposit amount), each
  with its own visited set: the first edge (in BFS order) to reach a node claims it, and the
  node's carried value is *that claiming edge's amount* -- not a sum over paths, not a min/max
  over paths. Claim order depends on FIFO queue order plus per-expansion edge ordering, so no
  path-enumeration + aggregation reproduces it (enumeration also explodes on hubs/cycles;
  that's rcp 075's conclusion too).
- **Ordered, truncated fan-out.** Each expansion sorts the node's eligible out-edges
  (union of rel types, optional rel-prop range filter -- the FinBench time window) by amount and
  truncates to TRUNCATION_LIMIT (10000) before claiming -- FinBench's spec-level truncation
  strategy, applied per hop. Which edges survive truncation changes which node claims which
  neighbor first: the ordering is part of the semantics, not a perf detail.
- **Cross-seed merge.** Across the per-seed BFS runs a node accumulates: inflow = sum of the
  per-seed claimed values, distance = min. (Seeds sit at depth 1; expansion while depth < 3.)
- Spec details that stay in the *query*, not the primitive: ratio = round3(inflow/loanAmount);
  final ORDER BY dist DESC, ratio DESC, id ASC; the pinned refs' ASCENDING fan-out order (the
  rust reference compares its truncation order case-sensitively, so "desc" falls through to
  asc -- if they regen the refs, the query text flips one argument, the engine doesn't change).
- Consciously out of scope v1: the spec's relative gate (edge amount > theta * upstream inflow)
  -- theta is 0 in the pinned workload, so it reduces to `amount > 0`, which the minValue
  parameter covers. If a workload ever sets theta > 0, add an optional relative-threshold
  option then.

## Design

**1. Core analytics kernel (public API -- version bump on land).** Next to BFSDistances/SSSP:

```go
type PropagateSeed struct { Node NodeID; Value float64 }
type PropagateOpts struct {
    RelTypes  []string   // fan-out union, e.g. transfer+withdraw
    Direction Direction
    MaxDepth  uint32     // seeds at depth 1; expand while depth < MaxDepth
    ValueProp string     // rel float prop carried by a claiming edge ("amount")
    Order     SortOrder  // fan-out order by ValueProp: Asc | Desc
    TruncLimit int       // 0 = no truncation
    MinValue  float64    // exclusive gate on the carried value (CR8: 0)
    FilterProp string    // optional rel-prop range filter (time window)
    FilterMin, FilterMax int64
}
func (g *Snapshot) PropagateBFS(seeds []PropagateSeed, opts PropagateOpts) []PropagateResult
// PropagateResult { Node NodeID; Value float64; Depth uint32 }
```

Every knob is generic graph structure (rel types, direction, depth, a rel prop, an order, a
limit); nothing recognizes a query. The test for generality: SPB/BI money-flow, taint
propagation, or any "trace X through typed weighted edges with per-hop budget" hits the same
kernel.

**2. gql surface: `CALL algo.propagate(...)` -- needs correlated CALL args (the real grammar
work).** Procedure CALL args are literal-only today (`litarg`; algo.bfs takes an *integer node
id*). CR8's seeds are runtime values from a MATCH, so this task grows procedure CALL args to
expressions over the in-scope variables, evaluated per incoming row, yields cross-joined with
that row. That is a general conformance/usability win on its own (algo.bfs(n) with a bound n,
fts.search with a computed term, ...) and is the alternative to encoding a seed-anchor spec
(label + key + seed-rel-type) into the procedure's literal args, which would smuggle CR8's
loan->deposit shape into the engine -- rejected as overfitting-adjacent.

```
CALL algo.propagate(seeds, seedValues, ['transfer','withdraw'], 'out', 3, 'amount', 'asc', 10000)
  YIELD node, value, depth
```

(seeds: list of nodes; seedValues: parallel float list -- both bound upstream. Three-column
YIELD; the search/algo procs yield one or two today.)

**3. CR8 in pure GQL then reads** (parameters inlined as the manifest does):

```
MATCH (loan:Loan {id: <seedLoan>})-[d:deposit]->(acct:Account)
RETURN collect(acct) AS seeds, collect(d.amount) AS seedValues, loan.loanAmount AS loanAmount
NEXT
CALL algo.propagate(seeds, seedValues, ['transfer','withdraw'], 'out', 3, 'amount', 'asc', 10000)
  YIELD node AS dst, value AS inflow, depth AS dist
RETURN dst.id AS dstId, round(inflow / loanAmount, 3) AS ratio, dist AS distanceFromLoan
ORDER BY distanceFromLoan DESC, ratio DESC, dstId ASC
```

Everything stateful is in the primitive; selection, arithmetic, and ordering stay plain GQL.

## Work items (post sign-off)

1. Core `PropagateBFS` + unit tests (hand-checkable small graphs: claim-order cases, truncation
   boundary, multi-seed merge, depth cap) -- ~200 lines.
2. Correlated expression args for procedure CALL: grammar (litarg -> expr), plan (defer arg
   binding), exec (evaluate per row, cross-join yields). Keep literal fast path. GRAMMAR.md.
3. `algo.propagate` plan/exec + 3-col YIELD + EXPLAIN rendering.
4. Verify: cr8 GQL text vs `python/refs/finbench/cr8.rust.json` on finbench_sf10_canonical.rcpg;
   full parity gate; FuzzQuery; public-API drive. Ping the verified text to their tasks/295.
5. Tag (public core API grew).

## Sign-off questions (Eve)

1. Core gets the PropagateBFS analytics kernel (public API, version bump)?
2. Correlated (expression) args for procedure CALL -- approve the general grammar change?
3. The primitive's semantics knobs as above -- general enough under the no-overfitting rule?
4. Name: `algo.propagate` (proposed) vs `algo.traceFlow` / `algo.claimBFS`.
