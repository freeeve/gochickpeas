---
name: verify
description: Verify gochickpeas engine/gql changes end-to-end -- parity gate against real LDBC data plus a public-API sample drive.
---

# Verifying gochickpeas changes

The surface is the library's public API. Two handles, use both for
nontrivial engine/gql changes:

## 1. Parity gate (real data, full pipeline)

```bash
go run ./cmd/gqlbench -manifest ~/rustychickpeas-ldbc/viz/data/gql_variants.tsv \
  -verify-only -cached-parity \
  -plans-golden cmd/gqlbench/testdata/plans_golden.txt
```

Expect `89/89 MATCH, 0 DIFF, 0 SKIP`, plus `plan-shape golden: 89 queries
unchanged`. This drives parse -> plan -> exec over SF1/FinBench SF10 exports
with pinned row hashes. Loads ~26M rels; takes a few minutes. Never emit
(-verify-only) from a dirty tree -- the append-only bench-out protocol stamps
engineCommit.

- `-cached-parity` also checks the auto-parameterized PlanCache path against
  the same reference hashes (catches literal-vs-cached-plan divergence).
- `-plans-golden` guards plan QUALITY, which row parity cannot see: a planner
  change that stays correct still moves the plan, and drift here fails the run.
  For an INTENTIONAL planner change, review the drift, then regenerate the
  golden with `-plans-golden-capture` and commit it in the same change. Do a
  planner change WITHOUT this and a regression that stays row-correct lands
  invisibly.
- Run heavy invocations under `taskman lock run local-cpu` (shared box).

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
- Timing on this machine is very noisy; only alloc counts (gqlbench
  profiles output) are a trustworthy A/B signal. Interleave A/B runs
  and keep the machine quiet.
- `gqlbench` must run from the repo root (HeadStamp shells to git);
  point -out/-plans-out/-profiles-out at the scratchpad.
- 45s of `go test ./gql -fuzz FuzzQuery -fuzztime 45s` is cheap
  insurance after recognizer/planner changes.
