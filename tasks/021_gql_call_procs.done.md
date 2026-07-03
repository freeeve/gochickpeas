# 021 -- gql CALL procedures (M19)

Execute CALL procedures by dispatching to the engine kernels through the
Native capability: per-node analytics (wcc, algo.wcc/pagerank/cdlp/lcc/
sssp/bfs) cross rows with one row per node; index-backed searches
(fts.search, geo.withinRadius/withinBBox) yield hit nodes ascending. The
weighted ANY SHORTEST form (SpStage.Weight) runs a hop-capped Dijkstra
keyed on (node, hops) with all three CostSpec kinds. The executor's last
checkSupported gates are removed -- every plan construct now executes.

Gate: kernel cross-checks under the dual-path harness; weighted-path
plan-IR tests; suite green -race; >=80% coverage.
