# 018 -- gql compile (M16)

Port the columnar compiled expression path (rustychickpeas-gql
src/compile.rs) into gql/internal/compile: Expr -> CExpr with resolved row
slots and typed Snapshot column readers, ceval mirroring the interpreter
exactly, IN-list hoisting, Slots() pushdown analysis. exec's RowEval seam
selects compiled when the graph asserts Native (eval.Ctx gains a
ForceInterp test hook so the dual-path harness can run both).

Gate: every root execute test runs under BOTH eval paths with identical
rows; FuzzEvalDiff (interpreted == compiled over generated exprs); >=80%
coverage; -race green.
