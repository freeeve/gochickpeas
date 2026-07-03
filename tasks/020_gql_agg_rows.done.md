# 020 -- gql aggregation + row stages (M18)

Port the executor's aggregation (Aggregator: count/sum/avg/min/max/collect
with DISTINCT, implicit group-by-non-agg-keys, post-aggregate scalar
wrappers/hidden slots) and the row-source stages: FOR (unwind) and CALL {}
correlated subqueries. Remove the corresponding checkSupported gates.

Gate: the Rust execute.rs aggregation subset + api_and_aggregates.rs
translated to GQL under the dual-path harness; suite green -race.
