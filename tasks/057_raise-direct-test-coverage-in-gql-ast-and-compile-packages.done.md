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

## Outcome (2026-07-10, commit 7f9130e)

First finding: the opening numbers were wrong. The 40.7%/51.2% figures
were FUNCTION-count averages, dragged down by ast's ~40 one-line sealed-
interface markers (isExpr/isClause) that nothing should ever call.
Statement-weighted -- what the CI gate actually measures -- ast was
already at 82.5%; only compile (68.7% of 530 stmts) was genuinely under
the bar.

So the work was compile-only: cover_test.go drives constExpr's arms
(list-scope bound variables through boundWith, previously 0%),
packNodeKey's four packing shapes, cevalProp's map/temporal/fallback
branches, propValue's kind decoding, the three IN-membership
representations under both hoist rewrites (batch-constant, carried, and
the carried epoch short-circuit), the slot walker over composite cnode
trees including the fused comparison node, and correlatedSlots'
collector arms with both bailouts (nested subquery, pattern
comprehension).

compile: 68.7% -> 82.6% statements; suite total 87.5%. Every engine
package now clears 80% statement-weighted. Test-only change: no parity
gate needed, no tag.
