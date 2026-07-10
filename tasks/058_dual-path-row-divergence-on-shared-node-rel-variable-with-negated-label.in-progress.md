# 058 -- dual-path row divergence on shared node-rel variable with negated label

Opened 2026-07-10. Found by FuzzQuery during 056's insurance run;
PRE-EXISTING (reproduces with 056's changes stashed), so 056 landed
without it and the failing seed was NOT committed to the corpus (a
committed seed is a red test in CI until fixed -- re-add it as part of
this fix).

## Repro

    MATCH(A:!A)-[A]-()RETURN(0)

`go test ./gql -run FuzzQuery` diverges on row count between the
interpreted (forceInterp) and compiled paths once this input is in the
fuzz corpus (seed id was 8842cf99b6d5851e).

The shape: one variable `A` bound as BOTH the node (with negated label
`:!A`) and the relationship of the same pattern -- a same-name
node/rel binding corner, presumably combining with the negated-label
filter differently in the two paths. GQL semantics for reusing a
variable across element kinds in one pattern likely make this an
equijoin (or a semantic error at bind time); whichever is right, both
paths must agree, and if it should be rejected, the parser/semantics
layer should reject it deterministically.

## Outcome (2026-07-10)

Root cause: the planner keys slots by NAME alone -- `A`-as-rel silently
reused `A`-as-node's slot, and the two execution paths disagreed on
which binding survived at label-filter time. GQL gives a variable one
element type, so the fix is deterministic rejection: a new
checkPatternVarKinds pre-pass over each segment's pattern specs claims
every named variable as node, relationship, or path, and any cross-kind
reuse is a bind error (same-kind reuse -- the established rebind/join
semantics -- is untouched, and no suite or manifest query relied on
cross-kind reuse).

Verified: the fuzz seed re-added to the committed corpus (passes -- both
paths reject identically); explicit TestErrorKinds cases for all three
directions (node->rel, rel->node, path->node) plus a same-kind-legal
guard; gate 89/89 MATCH; 90s extended FuzzQuery clean. Noted, not
covered here: subquery patterns (EXISTS/COUNT) bind through the
semantics scope rather than segment specs, so the same cross-kind claim
there would be a separate small pass if the fuzzer ever surfaces it.
