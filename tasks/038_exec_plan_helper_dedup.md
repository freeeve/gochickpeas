# 038: dedup exec/plan helpers that can drift

Review found five confirmed/plausible duplications inside the gql engine
where one fix applied to a single copy silently diverges the other:

1. `slotConstantSeeded` (gql/internal/exec/segment.go:193-211) vs
   `slotConstant` (stage.go:105-115): same "do all rows agree on this slot"
   logic, divergent out-of-bounds policy (false vs zero-Value compare).
   One helper with an explicit out-of-range policy.
2. `splitConjuncts` (exec/stage.go:118-125) vs `splitAndRef`
   (plan/lower.go:349-356): byte-identical AND-chain flatteners; exec
   already imports plan -- export one.
3. shortest-path BFS core triplicated (gql/internal/exec/shortest.go):
   `shortestPath` (~139), `buildSPTree` (~189), `allShortestPaths` (~230)
   hand-roll the same visited/parent/frontier/spCap loop over
   filteredNeighbors, differing parametrically (early-exit / parent links /
   dist map). Extract the frontier-walk core so hop-filter and depth-bound
   semantics stay in one place.
4. `projSink` DISTINCT (exec/project.go:63-72) keys whole rows on
   concatenated value.AppendKey while `distinctSet.add` (aggregate.go:129)
   dedups values with the u32 entity-id fast path. Not a drop-in (row vs
   value granularity), but single-column `RETURN DISTINCT <node>` should hit
   the entity fast path, and both depend on AppendKey's entity encoding --
   centralize that dependency.

Parity gate green after each extraction.

Note (2026-07-05, post-033/035): line numbers above predate commits
ceeed71/99b16e9 -- stage.go gained monoFilters in stageComp and
varexpand.go was restructured, so re-grep the symbols. All four
duplications still stand (none were touched). If extracting the shortest.go
BFS core, consider also threading the per-stage RelMatcher through
filteredNeighbors the way 035 did for the trail dfs (shortest.go still
resolves types per call). The parity gate command:
`go run ./cmd/gqlbench -manifest ~/rustychickpeas-ldbc/viz/data/gql_variants.tsv -verify-only`.
