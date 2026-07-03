# 015 -- gql plan (M13)

Port the planner from rustychickpeas-gql (per the approved plan; cost
branch hard-wired as the only strategy):

- gql/internal/plan: operator IR (ScanSource/BindOp/Stage/Segment/Plan,
  minus the recognizer kernel specs), Build/BuildWithInCols/planPart,
  buildSegment with the cost anchor ladder (rank tiers -> exact
  cardinality -> resolved first-hop degree -> avg-degree fan-out),
  lowering (seeks, hops, projection binding, nested-agg hoisting),
  cost probes (abstaining on params), join reorder + interior-anchor
  split, monotonic pushdown (inline + derived + violation forms),
  path-search and CALL-proc lowering, projection-before-aggregate
  fusion, and cardinality estimates + anchor notes.
- gql/internal/explain: the plan-tree text renderer (estimates always;
  PROFILE zip seam for M20).
- gql/internal/graph/types.go: NodeID/Direction re-exports so plan/exec
  need not import the engine directly.

Gate: plan-shape tests via IR + EXPLAIN goldens; plan coverage >=80%;
suite green under -race.
