# 016 -- gql eval (M14)

Port the interpreted expression evaluator (rustychickpeas-gql src/eval.rs +
temporal.rs) into gql/internal/eval: Ctx (explicit params, replacing Rust
thread-locals), core Eval dispatch, scalar-function library (FuncOp),
temporal math, EXISTS/COUNT subquery matching incl. nodeMatches (moved here
from exec to break the Rust eval->exec cycle).

Gate: Rust eval_functions.rs tests translated to GQL; null-propagation and
coercion tables; temporal edge cases; >=80% coverage; suite green -race.
