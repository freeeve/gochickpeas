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
