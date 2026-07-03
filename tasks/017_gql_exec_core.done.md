# 017 -- gql exec core (M15)

Port the executor's core pipeline (rustychickpeas-gql src/exec.rs, minus
recognize-kernel dispatch, expand/var-expand [M17], aggregation [M18], CALL
[M19]): segment runner, all scan sources, WHERE pushdown buckets, projection,
ORDER BY / top-k / OFFSET / LIMIT, UNION combine; interpreted eval behind the
RowEval seam; root package Run/RunWithParams/Explain wiring end-to-end.

Gate: ~50-60 translated execute.rs tests (single-hop MATCH/WHERE/RETURN/
ordering/pagination/params), exec unit tests; >=80% coverage; -race green.
