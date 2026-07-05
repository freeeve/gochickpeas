# 035: bounded-trail dfs path -- per-stage compile and scratch reuse

The 028 rounds gave the varReach BFS path persistent scratch
(reachScratch, matchloop.go:22) but the bounded dfs path (Min!=0 && Max!=nil)
still allocates per row. Review confirmed four independent wins in
gql/internal/exec/varexpand.go and the graph seam:

1. Mono filter compiled per row (varexpand.go:89-96): each
   varExpandCandidates call builds a new scope map, heap ast.Prop,
   monoFilter, and a compileEval tree. The sibling hop predicate compiles
   once per stage via buildHopFilters (stage.go:32) -- do the same for the
   mono key reader.
2. varWalk allocated per row (varexpand.go:79): pathRels/used/visited/
   scratch/nbufN/nbufP start nil and regrow every row, then the struct is
   discarded. Persist a walk scratch on the stage ctx like reachScratch and
   clear/reslice between rows.
3. DedupEndpoints map per row (varexpand.go:102):
   `seen := make(map[graph.NodeID]struct{}, ...)` per row; reuse a cleared
   scratch map (pattern at varexpand.go:136-139).
4. Rel-type re-resolution per hop: the dfs path passes raw w.op.Types so
   AppendNeighborsByType re-resolves names via s.g.Match(types...) on every
   call (gql/internal/graph/snapshot.go:152); the per-stage
   sc.relMatchers/CompileRelMatcher (stage.go:42) already exists and the
   reach path uses it (varexpand.go:151). Store rm on varWalk and use the
   Matched variant; same for reverseNeighborSet (expand.go:114).

Note (refuted candidate, do not chase): AppendRelationships' range over
iter.Seq does NOT heap-escape its yield closure -- escape analysis shows
`func literal does not escape` at snapshot.go:158.

Parity gate green after each change; re-measure the gql suite allocs.

## Resolution (2026-07-05)

All four landed, plus two adjacent wins the matcher threading exposed:

1. buildMonoFilters compiles the mono key reader once per stage
   (stageComp.monoFilters, mirroring buildHopFilters); the per-row compile
   block in varExpandCandidates is gone.
2. varWalk is persistent on genScratch, reset in place per row -- the six
   scratch buffers keep their backing arrays across the row loop (same
   one-live-walk-per-stage rationale as reachScratch).
3. The DedupEndpoints set is a cleared genScratch.dedup map instead of a
   per-row allocation.
4. The dfs and the fixed-hop named-rel expand now use a new seam method
   AppendRelationshipsMatched (core RelsMatch is a thin inlinable closure
   constructor, so the range stays allocation-free), and the dfs neighbor
   branch uses AppendNeighborsMatched -- both through the per-stage
   sc.relMatchers instead of re-resolving op.Types per hop.
   reverseNeighborSet/semijoinCandidates take the stage matcher too. The
   now-unused AppendRelationships(types) seam method was removed;
   AppendNeighborsByType stays (eval/subquery.go still resolves per call
   -- that path has its own shape cache and is out of scope here).

CR1 allocs unchanged (652,612 =~ the 653k in 028 -- its residue is
surviving-path ts materialization, not these paths); the wins apply to
row-heavy bounded-trail stages generally. Gates: full suite green, parity
49/49 MATCH, gofmt -s clean.
