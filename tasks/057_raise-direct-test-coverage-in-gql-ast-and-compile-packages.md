# 057 -- raise direct test coverage in gql ast and compile packages

Opened 2026-07-10, from the CI coverage-gate diagnosis.

Per-package function coverage (measured at 2d03469, -coverpkg over the
module): gql/internal/ast 40.7% (53 funcs), gql/internal/compile 51.2%
(46 funcs) -- the only engine packages below the 80% bar; everything
else reads 83-100%. The suite total (86.9% once the unmeasurable-in-CI
packages are excluded) hides them.

Likely gaps: ast walk/printer helpers unused by current tests; compile's
interpreter-fallback cnode branches (cSlow shapes, cCase/cFunc corners)
reached only when specific query forms compile. Write direct unit tests
per package -- table-driven over AST forms for ast, compiled-vs-
interpreted equivalence over the cnode surface for compile (the dual-
path harness in gql's execute tests is the pattern to borrow).
