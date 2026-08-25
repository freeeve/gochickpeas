---
name: verify
description: Verify gochickpeas engine/gql changes end-to-end -- parity gate against real LDBC data plus a public-API sample drive.
---

# Verifying gochickpeas changes

The surface is the library's public API. Two handles, use both for
nontrivial engine/gql changes:

## 1. Parity gate (real data, full pipeline)

```bash
GOCHICKPEAS_GQL_MANIFEST=~/rustychickpeas-ldbc/viz/data/gql_variants.tsv \
  go test ./internal/ldbc -run TestGQLParityGate -count=1 -v
```

Expect `89/89 MATCH, 0 DIFF, 0 SKIP`, plus `plan-shape golden: 89 queries
unchanged`. This drives parse -> plan -> exec over SF1/FinBench SF10 exports
with pinned row hashes, verifies the PlanCache (cached-parity) path against
the same hashes, and diffs every plan shape against the golden at
internal/ldbc/testdata/plans_golden.txt. Loads ~26M rels; takes a few
minutes. After an INTENDED planner change, review the drift and regenerate
in the same commit with GOCHICKPEAS_PLANS_GOLDEN_CAPTURE=1. Timings and
emission live in rustychickpeas-ldbc's sweep (their go/cmd drivers); this
gate is verification only, so no dirty-tree emission hazard exists.

- `-cached-parity` also checks the auto-parameterized PlanCache path against
  the same reference hashes (catches literal-vs-cached-plan divergence).
- `-plans-golden` guards plan QUALITY, which row parity cannot see: a planner
  change that stays correct still moves the plan, and drift here fails the run.
  For an INTENTIONAL planner change, review the drift, then regenerate the
  golden with `-plans-golden-capture` and commit it in the same change. Do a
  planner change WITHOUT this and a regression that stays row-correct lands
  invisibly.
- Run heavy invocations under the shared-box lock -- and not just the gate:
  full builds, full test runs, fuzz runs, and benchmarks are exactly the
  multi-core work that lands on top of an ldbc sweep mid-measurement.

  ```bash
  taskman lock run -ttl 20m -wait 30m -reason "<what>" local-cpu -- go build ./...
  taskman lock run -ttl 20m -wait 30m -reason "<what>" local-cpu -- go test ./...
  ```

  Incremental builds, `go vet`, and editor-driven builds are fine unlocked.
  A non-zero exit from `lock run` means another session holds the box: wait
  or do unrelated work -- never run the job anyway.

## 2. Sample drive through the public export

Scratchpad module with a replace directive resolves the local repo:

```
module verifydrive
require github.com/freeeve/gochickpeas v0.0.0
replace github.com/freeeve/gochickpeas => /Users/efreeman/gochickpeas
```

Build a small graph with `chickpeas.NewBuilder` (AddNode/SetProp/AddRel/
SetRelPropAt, then `Finalize("name")`), run queries with `gql.Run`, and
inspect plans with `gql.Explain` (e.g. grep the `[mono ...]` marker on
VarExpand lines to see whether a pushdown fired). Probe near-miss
phrasings and malformed input; errors should be clean plan/bind errors.

Gotchas:
- Timing on this machine is very noisy; alloc counts (MemProfileRate=1
  GC-synced diffs, or ldbc.MeasureAllocs) are the most trustworthy A/B
  signal. For timing A/Bs, run the
  whole comparison inside ONE `taskman lock run local-cpu` session in
  ABBA order (new-old-old-new): interleaving alone does not cancel a
  load trend, only ABBA does. A run whose TIMING is the product must
  also pass `-max-load 2` and must not publish on a non-zero exit.
- The parity gate runs from any cwd (paths resolve via the env var);
  timings/emission are the rustychickpeas-ldbc sweep's job now.
- 45s of `go test ./gql -fuzz FuzzQuery -fuzztime 45s` is cheap
  insurance after recognizer/planner changes.
