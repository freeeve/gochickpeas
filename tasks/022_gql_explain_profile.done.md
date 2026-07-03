# 022 -- gql EXPLAIN/PROFILE (M20)

PROFILE execution: thread per-operator produced-row counters through the
executor (per-op bind counts before level filters + a stage-WHERE survivor
slot, single-output counts for SP/CALL/FOR/subquery stages, projection and
boundary-WHERE row counts), zip them into the rendered plan tree
(branch-major, segment-minor), and surface PROFILE queries through the
public API as annotated one-column plan rows.

Gate: Rust profile cardinality + pushdown-pruning assertions translated to
GQL; explain package >= 80% coverage; suite green -race.
