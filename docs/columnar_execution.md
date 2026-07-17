# Columnar batch execution -- design and spike scope

Status: DESIGN + SPIKE SCOPED (2026-07-17). Owner: gql executor.
Prereqs landed: packed group-key slot probe (5a03f84), bare-var aggregate
argument slot reads (10f25e5) -- the row-at-a-time forms of the same idea.

## Problem

The gql executor runs row-at-a-time over boxed `value.Value` rows. On
flow-bound queries -- large chain walks feeding aggregation -- that model
is ~25x the hand-written native floor (Q4: ~954ms gql vs 39ms native for
the SAME ~4.7M-row traversal; the anchor choice is proven optimal, the
volume is intrinsic). Q4's profile: ~58% genMatches walk, ~24% aggregate
probe, ~13% sort -- all of it per-row interface dispatch, `value.Value`
boxing, and arena copies (1.13GB/run of arena churn on Q4 alone). The
standing goal is gql < 5x native on every query; the flow-bound class
cannot get there by shaving per-row constants -- the row protocol itself
is the cost.

Evidence bounding the alternatives (task 205):
- Selectivity-aware anchor planning: falsified -- the current anchor is
  already optimal on the flagship; no target.
- Thread-parallel terminal aggregation: measured negative on the rust
  sibling (rcp-411); the shape is memory-bandwidth-bound, so more cores
  saturate, they don't help.
- Narrow batch aggregation alone: attacks only the 24% share; ceiling too
  low to justify standalone. It is folded into this design's spike.

## Design

### Batch representation

A `colBatch` is a struct-of-columns snapshot of up to `batchCap` (~2048)
in-flight rows, replacing `[]value.Value` rows on eligible paths:

- Entity columns: `[]uint32` ids plus a column kind (node/rel) -- the
  dominant column type in walk-heavy segments.
- Scalar columns: typed `[]int64` / `[]float64`; strings as offsets into
  a per-batch byte arena.
- A validity bitmap per column (OPTIONAL nulls).
- Columns exist only for LIVE slots (slots read downstream); dead
  intermediate slots are never materialized.

No selection vector in the spike: filters compact during the gather that
builds the next batch (compaction is one pass the gather already makes).

### The seam

Today every stage boundary is `rowSink.push([]value.Value)`. Batch-capable
operators implement a parallel `batchSink { pushBatch(*colBatch) bool }`.
Exactly one bridge exists: a boxing adapter that expands a batch into rows
for a non-capable downstream sink (the spill boundary). There is no
row-to-batch bridge -- a segment either starts batched at its scan or runs
row-at-a-time unchanged. This keeps the two protocols from interleaving
mid-chain and keeps the fallback story trivial: ineligible = the exact
current engine.

### Eligibility (structural -- the no-overfit gate)

Batch execution fires on STRUCTURE, never on query identity, in keeping
with the repo's overfitting rule. A MatchStage chain is batch-eligible
when:

1. Every op is `OpScan` or `OpExpand` (no var-expand, no optional, no
   named paths) with untracked rel-uniqueness, or uniqueness whose check
   window is a single hop (checkable during the gather).
2. Every WHERE conjunct pushed into the chain reads only slot columns
   through predicates the vectorizer covers (comparisons over entity ids,
   ints, floats, day-ints; everything else keeps the row path).
3. The terminal consumer is an aggregation whose group keys and arguments
   resolve to slots (the 5a03f84/10f25e5 machinery -- the batch update is
   its vector form), or a projection into ORDER BY.
4. The segment's output order is unobservable: an ORDER BY is present, or
   the projection is a single keyless aggregate. This is required because
   batched expansion emits level-order, not DFS order, and the aggregator's
   group ENCOUNTER order is observable in unordered results. (Every LDBC
   benchmark query orders its output; unordered queries simply keep the
   row path.)

The eligibility test lives in the planner as a pure function of the lowered
ops -- an unseen query with the same structure batches identically.

### Batched operators (spike set)

- **Scan**: anchor candidates emitted as entity-column batches (label
  sets and prop-value indexes already produce id runs).
- **Expand**: for each batch, gather neighbors per source row from the
  typed CSR into the next batch (prefix-carrying columns copy through;
  compaction applies hop label/prop filters inline). Level-batched,
  anchor-major.
- **Aggregate update**: pack group-key entity columns into a `[]uint64`
  keys vector, then one tight probe loop into the existing `indexI`/slab
  state -- no per-row dispatch, no `value.Value` in the loop. count/sum
  read typed argument columns directly.
- **Boxing adapter**: batch -> rows for any downstream the spike does not
  cover (ORDER BY consumes the aggregator's finalize, which is already
  post-batch).

### What batching buys, concretely

Per row today: sink dispatch + arena copy + per-key eval/copy + per-agg
dispatch. Per row batched: one gathered id write + amortized (1/batchCap)
loop overhead. The walk's CSR reads remain -- they are the irreducible
memory traffic -- but the surrounding interface machinery, bounds-checked
one-value-at-a-time copies, and GC pressure (Q4's 1.13GB/run) go away.

## Spike (phase 0) -- build-to-measure

Vertical slice, parity-gated, on a research branch until the verdict:

1. `colBatch` + planner eligibility gate + batched Scan/Expand/AggUpdate
   + boxing adapter.
2. Measure the eligible flow-bound set (Q4 and whichever of Q6/IC9/CR-class
   the gate admits) A/B under a quiet box; full 89/89 parity plain+cached
   and plan goldens throughout.

Success: Q4 wall >= 2.5x faster (~954 -> under ~400ms) with parity green.
Fail-out: if the batched walk beats row-at-a-time by < 1.5x on Q4, the
bandwidth-bound hypothesis wins -- record the numbers, preserve the branch
per the research-branch convention, and stop; the remaining lever for this
class would be representation work in core (CSR layout, prefetch-friendly
ordering), not executor protocol.

## Phasing (post-spike, each parity-gated)

1. Coverage: bounded var-expand (trail batches), OPTIONAL (validity
   masks), vectorized WHERE beyond comparisons.
2. Batch projection + typed sort keys (the 13% sort share; composes with
   the existing top-k gate).
3. Spill-boundary erosion: batch DISTINCT, batch hash-join probe (the
   058361a table is already flat/chain-form and batch-friendly).

## Risks

- **Go, no SIMD intrinsics**: wins must come from dispatch elimination,
  bounds-check elimination, and cache locality, not vector units. The
  spike's fail-out criterion exists precisely for this.
- **Order semantics**: covered by eligibility rule 4; any future widening
  must re-derive observability, not relax it.
- **Rel-uniqueness in batches**: full multi-hop check windows are per-path
  state; spike excludes them (rule 1) rather than approximating.
- **Two protocols to maintain**: bounded by the single-bridge rule and by
  the parity gate running every query through whichever path the gate
  assigns it -- the fast path can never silently diverge.
