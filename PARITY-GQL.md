# GQL engine parity checklist

Hand-maintained rows for `rustychickpeas-gql`'s public API (its exports are
mostly types and traits, so the function-scraping `scripts/parity.sh` does
not apply; source of truth is that crate's `src/lib.rs`). Statuses: done /
wontport (with a reason). The Go surface speaks ISO GQL rather than Cypher;
behavioral parity is checked query-by-query via `gql/testdata/xcheck/`.

| rust symbol | go symbol | status | notes |
|-------------|-----------|--------|-------|
| `run` | `gql.Run` | done | GQL surface; pipeline parse -> desugar -> plan -> execute |
| `run_with_params` | `gql.RunWithParams` | done | named `$name` params; unsupplied reads null |
| `explain` | `gql.Explain` (+ `EXPLAIN` query mode) | done | estimates + anchor notes render unconditionally |
| `CypherQuery::query` | `gql.Run` | done | extension trait folded into package functions |
| `CypherQuery::prepare` / `Prepared` | `gql.Prepare` / `gql.Prepared` | done | statistics-staleness caveat documented |
| `Prepared::execute` | `(*gql.Prepared).Execute` | done | EXPLAIN/PROFILE modes render on Execute |
| `CypherQuery::query_cached` | `(*gql.PlanCache).Run` / `RunWithParams` | done | both layers cache the template plan; no cost-mode bypass (cost probes abstain on params, M13 audit) |
| `CypherQuery::query_with_params` | `gql.RunWithParams` | done | |
| `PlanCache` / `DEFAULT_MAX_BYTES` | `gql.PlanCache` / `gql.DefaultCacheBytes` | done | byte-budgeted two-layer LRU; mutex-guarded |
| `CypherError` | `gql.ErrParse` / `ErrBind` / `ErrPlan` / `ErrEval` | done | `errors.Is`-able sentinels per stage |
| `Row` / `Rows` | `gql.Row` / `gql.Rows` | done | `Next`/`NextBatch`/`All` (`iter.Seq`) |
| `Value` | `gql/value.Value` | done | lifetime-free struct variant; exported subpackage |
| `parse` / `pub mod ast` | `gql/internal/parser.Parse` / `gql/internal/ast` | done | internal until an external consumer appears (mechanical promotion) |
| `CypherGraph` | `gql/internal/graph.Graph` | done | consumer-side seam over `*chickpeas.Snapshot`; executor hooks replaced by the `graph.Native` capability |
| `RowEval` / `InterpExpr` | `gql/internal/exec.RowEval` | done | interpreted + columnar compiled forms behind one seam |
| `CallProc` / `CallStage` / `SpStage` | `gql/internal/plan` | done | plan IR stays internal |
| `Direction` / `NodeId` (`types.rs`) | `chickpeas.Direction` / `chickpeas.NodeID` | wontport | engine scalar types reused; no cypher-owned twins needed (no wasm split) |
| `force_planner` / `PlannerMode` | -- | wontport | single cost-based planner; no mode switch exists |
| `run_call_generic` | -- | wontport | portable kernel twins existed for the wasm backend; CALL dispatches to chickpeas kernels |
| `shortest_path_cost_generic` / `shortest_path_const_cost_generic` | -- | wontport | same reason; weighted paths run the exec Dijkstra over seam weight readers |

Not exported by the Rust crate but tracked for context: `plan/recognize.rs`
benchmark-shape recognizers and every `Native`/`PortableKernel` plan variant
are wontport ("benchmark-shape recognizers; the generic pipeline is
result-identical").
