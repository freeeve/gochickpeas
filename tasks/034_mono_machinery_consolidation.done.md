# 034: consolidate the monotonic-pushdown machinery

## Resolution (2026-07-05)

Landed as designed, plus two correctness fixes the consolidation surfaced:

- One normalizer (`monoConjunctShape` -> `monoPairCore`, mono.go): both
  surface families (all() over consecutive pairs; violation-count
  size([..])=0) reduce to a canonicalized core -- affine index offsets
  (i, i+c, c+i, i-c), either operand order, range(lo, size(L)+m) with
  exact pair-coverage checks (inclusive range; all() must cover exactly
  pairs 0..size-2 since an out-of-range read nulls the quantifier; the
  violation form may over-cover upward since null is never a violation;
  lower bound exact in both because a negative index WRAPS). Handles both
  element modes (bare list alias, inline rels(e)[i].k) in the same core.
  Unseen equivalent phrasings (e.g. range(1, size(L)-1) with L[i-1]<L[i])
  now push. Pinned by TestMonoNormalizerPhrasings.
- One pass (`pushMonoPreds`, called from planPart after all segments
  build): inline stage-WHERE conjuncts push same-stage; boundary-WHERE
  alias conjuncts resolve through the defining projection walking di=fi..0
  (subsumes the old same-segment pushDerivedMonoPred as the di=fi case and
  the old cross-segment walk as di<fi). tryPushMonoPred (lower.go),
  pushDerivedMonoPred (build.go), pushCrossSegmentMono all deleted.
- Correctness fix 1 (OPTIONAL): the old derived/cross-segment pushes
  ignored MatchStage.Optional -- pruning an optional walk null-extends
  rows the (consumed) boundary filter would have dropped. resolveMonoTarget
  now bails on optional stages; inline same-stage pushes remain legal (a
  stage WHERE is scoped inside its own optional). Pinned at plan level
  (TestMonoOptionalStageNotPushed) and end-to-end
  (TestMonoOptionalWalkNotPruned, mutation-checked).
- Correctness fix 2 (rebound alias): the old cross-segment walk started at
  fi-1, never checking whether segment fi's own projection rebinds the
  alias (e.g. ts -> ts[0..2]) -- it would push against the original
  comprehension while the filter constrains the rebound value. The unified
  walk starts at di=fi, so any non-passthrough rebinding stops the search.
  Pinned by TestMonoReboundAliasNotPushed.
- Exec seam: hopGate{pred *hopFilter, carry *hopCarry} replaces the
  parallel hopFilters/monoFilters slices; dfs threads an opaque
  carryState{val, have} and hopCarry.step is the single extend-or-reject
  point. varReach takes the same gate and applies the stateless pred by
  walking relationships when present (carry stays planner-guaranteed nil
  there: reachable-set dedup has no per-path state). Future carried-state
  constraints implement step instead of new fields + params.
- weightExprCheck + weightSubqueryCheck + predRefsOnly folded into
  freevars.go: collectVars is the one exhaustive descent (locals-scoped
  free mode / over-collecting collectAllVars mode); validateWeightExpr and
  predRefsOnly are freeVarsOutside subset checks. Previously-"unsupported"
  kinds (Reduce, ListComp, Case in per-hop preds, ...) whose free refs are
  only the rel var now validate and evaluate via the general evaluator.

Verification: full suite green; parity gate 49/49 MATCH; CR1 allocs
652613 -> 652607 (neutral); pinning tests all green
(TestCrossSegmentMonoDropCorrectness, sparse/violation/dense/float/min-0).

---

Original task description below.

Was blocked on 033 -- now UNBLOCKED (033 done 2026-07-05, commit ceeed71;
035 done same day, 99b16e9). Line numbers below predate those commits.

## State to know before starting (post-033/035)

- The correctness contract is now: the walk prunes with EXACTLY the
  recognized filter's semantics, and every push form CONSUMES the conjunct
  (no redundant guard anywhere). Any consolidation must preserve that
  exactness -- the pinning tests are gql/mono_drop_test.go
  (TestCrossSegmentMonoDropCorrectness, TestMonoSparseKeyMatchesFilter,
  TestMonoViolationCountNullsPass, TestMonoDenseUnsetZeroSemantics,
  TestMonoFloatKeyMatchesFilter, TestMinZeroNamedPathRejected) and
  gql/internal/plan/mono_test.go.
- MonoHopSpec now carries NullsPass (per-shape null semantics: all() form
  prunes an incomparable pair, violation-count form passes it) -- a
  normalized "is sorted(L)" recognizer must keep emitting that bit per
  source shape. monoConjunctShape (mono.go) is the shared two-shape
  recognizer entry the derived + cross-segment passes already use.
- Min-0 gates live in tryPushMonoPred and applyMonoTarget (both bail;
  applyMonoTarget returns bool so callers keep the conjunct). A named path
  over min-0/unbounded quantifiers is rejected at build (build.go, next to
  the ACYCLIC guard), so min-0 mono is unreachable from the query surface.
- Exec side after 035: monoFilter compiles once per stage
  (stageComp.monoFilters via buildMonoFilters, next to buildHopFilters);
  varWalk persists on genScratch with an rm *graph.RelMatcher field and
  traverses via AppendRelationshipsMatched/AppendNeighborsMatched. The
  carry through dfs is (prevVal value.Value, havePrev bool); comparison is
  monoFilter.allows using value.Compare. The generalized per-hop
  carry+accept mechanism should subsume exactly this.
- Dense-vs-sparse missingness (tasks/041) means the same query changes
  results with column density -- walk==filter holds either way because
  both read through the same compiled propReader; keep it that way.

Review found the mono optimization is one logical rewrite fragmented across
~6 sites, and the recognizer set is accreting one matcher per surface
phrasing -- each fires on generic shape (so it passes the no-overfitting
rule) but the growth pattern is the wrong altitude.

## Findings

- Three syntactic matchers for the same "list is sorted" property in
  gql/internal/plan/mono.go: `monoIndexedProp` (rels(e)[i].k form, ~14/224),
  `derivedMonoShape` (L[i]<L[i+1] form, ~92), `violationCountMonoShape`
  (size([...])=0 form, ~128). An unseen equivalent phrasing (e.g.
  `all(i IN range(1,size(L)-1) WHERE L[i-1]<=L[i])`, or a reduce) misses the
  pushdown.
- Three disjoint passes at three pipeline stages: `tryPushMonoPred`
  (lower.go:243), `pushDerivedMonoPred` (build.go:140), `pushCrossSegmentMono`
  (plan.go:97) -- the last reaches into other segments' Stages/Slots/Proj
  (mono.go:323-334).
- Exec side: `monoFilter` is a bespoke varWalk field threaded through dfs via
  extra carried params (prevVal/havePrev, varexpand.go:212), parallel to
  hopFilter and absent from varReach.

## Direction

- One normalizer that answers "is this predicate equivalent to
  sorted(L)/reverse-sorted(L)" over a canonicalized AST, feeding one pushdown
  pass that runs over the fully built segment graph (covers same-segment and
  cross-segment uniformly).
- Generalize the exec seam to a stateful per-hop carry+accept mechanism that
  all walk modes share (varWalk dfs and varReach), so future carried-state
  constraints (sum-bounded, non-decreasing weight) reuse it instead of adding
  another struct + threaded params.
- Also from review: `weightExprCheck` (gql/internal/plan/sp.go:86-174)
  duplicates the exhaustive AST descent that `collectAllVars` (reorder.go:77)
  already implements, and unhandled kinds (ListComp, ListPred, Reduce,
  MapProj, HasLabelExpr, PatternComp) fall through to "unsupported weight
  form" -- fold it into a generic free-variable collector + scope-subset
  check while in here.
