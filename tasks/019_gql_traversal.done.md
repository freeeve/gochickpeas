# 019 -- gql traversal (M17)

Port the executor's traversal ops (rustychickpeas-gql src/exec.rs):
Expand (incl. bound-target rebind semijoins), VarExpand (bounded DFS
trails, unbounded/zero-length BFS reach with dedup, per-hop rel predicates,
mono pushdown), OPTIONAL MATCH, named-path binding + post-path filters, and
the ANY/ALL SHORTEST stages (BFS single + all shortest paths, weighted
form for the engine). Remove the corresponding checkSupported gates.

Gate: the Rust execute.rs expansion/path test subset translated to GQL
under the dual-path harness; suite green -race; >=80% coverage.
