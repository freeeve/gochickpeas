# 014 -- gql semantics (M12)

The passes between parsing and planning, ported from the Rust crate
(desugar.rs / binder.rs / autoparam.rs) into `gql/internal/semantics`:

- `Desugar`: normalizes the AST in place before planning (idempotent) --
  non-literal inline pattern properties lower to WHERE equality conjuncts
  on the (possibly synthesized `__ip{n}`) element variable, including
  inside EXISTS/COUNT subquery patterns; rejects prop-exprs on
  variable-length relationships (KindPlan).
- Binder helpers: `IsAggName`/`IsKnownScalarFunc`/`IsKnownFunction`,
  `DerivedName` column naming, `ExprHasAgg` (bounded at subquery scopes),
  `CheckRefs`/`CheckRefsSkippingAgg` reference validation with the exact
  Rust scoping rules (list predicates, reduce, comprehensions, subquery
  patterns, map projections).
- `AutoParameterize`: lifts inline property constants and WHERE comparison
  bounds into numbered param slots (left-to-right, stable), returning the
  values in slot order; CASE thresholds, function args, projection
  constants, FOR lists, CALL proc args, and quantifier bounds stay baked.

Gate: Rust autoparam tests ported (GQL surface), desugar goldens +
idempotence, binder scope/error tables; >=80% coverage;
`go test ./... -race` green.
