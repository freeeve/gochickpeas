# 028 -- allocation reduction: native kernels to ~zero-alloc, then a GQL pass

Filed 2026-07-03 per Eve: several queries show many allocations in the bench emissions,
GQL especially. Reduce allocations toward zero on the native ops first, then do a GQL
pass. Generalized engine wins are the goal -- do NOT over-fit to the LDBC queries; when a
clear win needs a core API change, make it (core has no external consumers yet), as long
as the surface stays conformant with GQL's standards.

## Method (iterate per round)

1. **Bench with allocs**: `go run ./cmd/ldbcnativebench -manifest bench-out/native_variants.tsv`
   appends per-query alloc profiles (`bench-out/profiles_gochickpeas.jsonl`, Mallocs/TotalAlloc
   deltas over one warm run). The rust floor for comparison is their
   `viz/data/profiles.jsonl` (`rcp-native (rust)` rows, counting-allocator allocs/bytes).
2. **Reduce**: attack the biggest allocators with general engine fixes (iterator reuse,
   scratch buffers on Snapshot readers, avoiding per-row boxing, string interning reads,
   set/map recycling). Track per query below; re-verify parity (89/89 must hold) after
   every change.
3. **Profile hot paths**: once allocs are down, `go test -bench`/pprof the remaining hot
   kernels and resolve CPU hot paths the same way (general wins first).
4. **GQL pass**: repeat 1-3 for `cmd/gqlbench` (plan/exec allocations; the deliberately
   deferred perf work in [[gql-port-progress]] -- streaming top-k/aggregate segments,
   bounded top-k heap -- is in scope if benchmarks point there).

## Per-query tracking

Populate from each profiles emission round (family/query: allocs -> after). Baseline is
the 2026-07-03 emission at 29284c3; fill top offenders first.

(baseline table to be appended after the first emission round)

## Constraints

- Parity gate is sacred: every optimization round ends with `-verify-only` 89/89 MATCH.
- Kernel code must stay readable as the ported reference (it is emitted to the viz as
  the code that ran); prefer engine/core improvements over kernel-local contortions.
- Public gql API (`Run`/`RunWithParams`/`Prepared`/`PlanCache`) and the rowhash encoding
  are integration surfaces -- don't break them.

### Baseline (emission at 29284c3, 2026-07-03) -- top 30 by allocs

| query | allocs | bytes | rows |
|---|---:|---:|---:|
| BI/Q15 | 34,694,274 | 1,012,225,064 | 1 |
| IC/IC5 | 12,864,253 | 395,305,256 | 20 |
| IC/IC3 | 10,655,771 | 331,541,520 | 20 |
| BI/Q2 | 6,078,097 | 208,545,840 | 100 |
| BI/Q6 | 5,760,484 | 150,232,544 | 100 |
| IC/IC6 | 3,641,088 | 144,453,608 | 10 |
| IC/IC12 | 3,630,682 | 137,337,840 | 20 |
| SPB/a13 | 1,311,207 | 84,551,928 | 336,315 |
| SPB/a14 | 1,259,844 | 39,673,592 | 23,192 |
| BI/Q4 | 547,811 | 20,915,800 | 100 |
| SPB/a5 | 519,710 | 21,190,104 | 108,476 |
| BI/Q9 | 490,543 | 12,295,600 | 100 |
| BI/Q14 | 423,001 | 12,911,776 | 7 |
| SPB/a7 | 331,598 | 11,716,960 | 33,561 |
| IC/IC10 | 284,055 | 32,385,056 | 10 |
| BI/Q3 | 247,030 | 6,634,224 | 20 |
| IC/IC4 | 229,196 | 6,705,536 | 10 |
| SPB/a25 | 228,120 | 19,931,280 | 47,499 |
| BI/Q18 | 200,770 | 21,631,320 | 20 |
| BI/Q12 | 189,790 | 18,034,896 | 86 |
| BI/Q10 | 169,497 | 30,207,248 | 100 |
| SPB/a19 | 156,073 | 5,969,408 | 11,434 |
| SPB/a10 | 151,176 | 7,494,936 | 16 |
| SPB/a9 | 151,115 | 4,080,200 | 1 |
| SPB/q5 | 118,504 | 4,550,912 | 7,898 |
| BI/Q17 | 114,085 | 4,673,400 | 10 |
| SPB/q9 | 113,706 | 4,462,720 | 9,462 |
| BI/Q7 | 107,870 | 3,912,072 | 100 |
| SPB/q7 | 75,486 | 2,452,880 | 4,641 |
| SPB/a6 | 67,097 | 2,028,128 | 3 |

Suite total: 85,342,501 allocs. Rust floor: BI ~0, IC 45-61k, IC12 ~1k.

### Round 1+2 (traversal accessors e1ab6f8^, Set.Iter e1ab6f8) -- emitted at e1ab6f8

| query | allocs 29284c3 | allocs e1ab6f8 | ms before | ms after |
|---|---:|---:|---:|---:|
| BI/Q15 | 34,694,274 | 18,729 | 114.24 | 35.92 |
| IC/IC5 | 12,864,253 | 2,012 | 692.82 | 513.68 |
| IC/IC3 | 10,655,771 | 249 | 350.86 | 205.60 |
| BI/Q2 | 6,078,097 | 5,409 | 20.47 | 9.32 |
| BI/Q6 | 5,760,484 | 2,746 | 142.50 | 41.10 |
| IC/IC6 | 3,641,088 | 3,669 | 110.79 | 45.19 |
| IC/IC12 | 3,630,682 | 1,375 | 99.37 | 48.54 |
| SPB/a13 | 1,311,207 | 1,009,015 | 62.36 | 56.79 |
| SPB/a14 | 1,259,844 | 46,413 | 36.24 | 18.74 |
| BI/Q4 | 547,811 | 11,132 | 78.04 | 71.73 |
| SPB/a5 | 519,710 | 217,505 | 64.07 | 53.13 |
| BI/Q9 | 490,543 | 42,820 | 17.04 | 12.13 |
| BI/Q14 | 423,001 | 2,943 | 13.85 | 7.49 |
| SPB/a7 | 331,598 | 67,401 | 21.29 | 15.90 |
| IC/IC10 | 284,055 | 1,823 | 10.36 | 7.18 |

Suite: 85,342,501 -> 2,267,808 allocs (97.3% reduction); summed warm medians 2156ms -> 1465ms.
Remaining allocs are dominated by result-row [][]any boxing (per-row, matches the rust side's own per-row materialization) and kernel-local maps.

### Round 3 (gql pass, commit 97cdc78) -- batch Append* on the Graph seam

Root cause found for the gql side: the executor traverses through the graph.Graph
INTERFACE, and an interface-returned iter.Seq can never devirtualize -- every expand /
var-expand hop paid heap closures per row. Fix: seam-conformant batch methods
(AppendNeighborsMatched / AppendNeighborsByType / AppendRelationships) filling
caller-pooled buffers; expandCandidates filters the appended tail in place, varReach and
varWalk.dfs reuse per-walk buffers.

| gql query | allocs eb74933 | after core fixes (669e756) | after batch (97cdc78) | ms 669e756 | ms after |
|---|---:|---:|---:|---:|---:|
| BI/Q6 | 58,432,686 | 40,293,667 | 22,097,892 | 1387 | 1094 |
| BI/Q18 | 29,929,298 | 28,586,884 | 28,517,896 | 4152 | (unchanged) |
| BI/Q12 | 21,211,214 | 21,211,213 | 3,268,572 | 1979 | 1565 |
| IC/IC5 | 14,115,179 | 14,074,932 | 3,425,714 | 3019 | ~3094 |
| IC/IC3 | 13,182,798 | 13,152,743 | 4,687,981 | 782 | 666 |
| IC/IC9 | 10,311,444 | 10,310,594 | 10,275,687 | 5186 | (unchanged) |
| IC/IC6 | 3,841,509 | 3,840,657 | 155,648 | 133 | 81 |
| IC/IC12 | 3,256,988 | 3,256,747 | 436,160 | 126 | 74 |

### Round 4 (in progress, 2026-07-03) -- streaming push-pipeline segments

Baseline re-emitted at 143e9ae (commit a252f48): top gql offenders Q18 28.5M, Q6 22.1M,
IC9 10.3M, CR1 4.9M, IC3 4.7M, Q1 3.9M, IC5 3.4M allocs.

Design (per Eve's windowed-arena direction; the window degenerates to per-stage row
buffers because every stage is per-row convertible): replace runSegment's
stage-by-stage `[][]value.Value` materialization with a push pipeline -- each stage
becomes a sink holding one reused full-width row buffer; genMatches already generates
into a sink by reference, so match rows flow straight through the chain with zero
per-row allocation. The aggregator is already a streaming accumulator (update/finalize)
and becomes the terminal sink for aggregated projections; non-aggregated projections
retain only projected rows (plus the matched row when an ORDER BY key is not a
projected column) in a chunked bump arena. Semantics preserved: depth-first push order
equals materialize-then-iterate order (group encounter order, DISTINCT first-occurrence,
stable-sort ties); OPTIONAL re-emit uses a reused orig buffer; PathBind becomes a
per-row wrapper; batch-constant hoisting is re-derived statically (const = unbound by
any stage in the segment and identical across the segment's seeded inputs); PROFILE
counters accumulate per stage across pushes. Deferred: LIMIT early-abort through the
sink chain (needs a stop signal), streaming distinct-agg value.Key.

Shipped in the same round, driven by alloc-site profiles (gqlbench gained -cpuprofile /
-memprofile flags):

- aggregator.update reuses group-key and DISTINCT-key scratch buffers (map lookups on
  string(scratch) don't allocate; only new-group/new-value inserts do).
- eval subqueries (EXISTS/COUNT/pattern comprehensions) cache their DFS shape per
  pattern on the Ctx -- anchor-reversal, level slots, extended scope, per-level
  candidate scratch, BFS sets, and the memoized unanchored level-0 scan -- validated
  against the outer scope per hit; neighbor iteration switched to the batch
  AppendNeighborsByType seam. Was Q18's whole profile (newSubqueryFrame 19.9M +
  dfs 12.7M + maps.Copy).
- cSubquery's memo key buffer reuses node-local scratch.
- cInCarried skips the per-epoch membership rebuild when the list is payload-identical
  to last epoch's (new O(1) value.SameBacking) -- recovers const-hoist behavior for
  segment-stable carried lists under the streaming pipeline's static const rule.
- reconstructPathNodes resolves each hop O(1) via RelEndpoints(pos) instead of
  scanning the node's adjacency for the position -- FinBench CR1's hub-node paths
  went 2227ms -> 175ms.

Timings wobbled mid-round because a concurrent rustychickpeas-bench run loaded the
machine (load avg 28); interleaved A/B and post-quiet reruns confirmed the wins.
TestPlanCacheConcurrent flaked once (Len()=0) -- reproduced on the UNMODIFIED
baseline worktree, pre-existing, needs its own look.

Results from the full 49-query emission at 56ce1a9 (49/49 MATCH; top 12 by baseline
allocs):

| query | allocs 143e9ae | allocs 56ce1a9 | ms before | ms after |
|---|---:|---:|---:|---:|
| BI/Q18 | 28,517,896 | 1,980,271 | 4266.25 | 3737.67 |
| BI/Q6 | 22,097,949 | 3,519,610 | 1314.86 | 868.04 |
| IC/IC9 | 10,275,687 | 9,228 | 6324.68 | 3817.77 |
| FinBench/CR1 | 4,945,885 | 2,951,396 | 2226.65 | 171.44 |
| IC/IC3 | 4,687,976 | 4,252,190 | 750.53 | 652.38 |
| BI/Q1 | 3,932,766 | 1,195 | 628.56 | 559.31 |
| IC/IC5 | 3,425,713 | 645,773 | 3296.13 | 3078.65 |
| BI/Q12 | 3,268,572 | 3,091,197 | 1816.76 | 1582.75 |
| IC/IC10 | 1,646,430 | 2,611 | 422.55 | 290.80 |
| IC/IC2 | 1,264,326 | 416 | 532.29 | 348.39 |
| BI/Q2 | 572,288 | 12,305 | 124.43 | 124.86 |
| BI/Q11 | 488,844 | 30,688 | 159.26 | 138.83 |

gql suite: 87,056,543 -> 17,455,156 allocs (80% reduction); summed warm medians
22335ms -> 15724ms (30% faster). v0.5.0 tagged (value.SameBacking is a public
gql/value addition).

### Round 5 candidates (next session: profile first, don't trust this ranking)

Remaining top allocators after round 4: IC3 4.25M, Q6 3.5M, Q12 3.1M, CR1 2.95M,
Q18 2.0M. Known threads to pull, in rough order:

1. Per-query -memprofile the five above (`go run ./cmd/gqlbench -manifest
   ~/rustychickpeas-ldbc/viz/data/gql_variants.tsv -only IC3 -runs 1 -memprofile
   /tmp/x.allocs -out /tmp/a -plans-out /tmp/b -profiles-out /tmp/c`, then
   `go tool pprof -sample_index=alloc_objects -top <binary> /tmp/x.allocs`;
   ignore load-phase rows: computeInToOutFromCSR, rcpg.*, ReadRCPG).
   Last combined profile's non-load leaders: value.AppendKey growth in
   DISTINCT-agg seen-set inserts (inherent unless keys go comparable),
   eval.extendRow / evalScalarFunc / evalListComp / applyRange (interpreted
   eval per-call slices), exec.varReach (per-call BFS state -- shape-cache
   like the eval subqueries), value.List in var-expand RelSlot binding.
2. Deferred streaming pieces: bounded top-k heap for ORDER BY+LIMIT terminals,
   LIMIT early-abort (sink push returning bool), UNION branch streaming.
3. Native side (round 2 leftovers): result-row [][]any boxing via flat
   chickpeas.Value arena rows (native kernels only, internal/ldbc surface);
   SPB a13 1.0M / a5 218k row-proportional.
4. TestPlanCacheConcurrent flake (Len()=0, pre-existing at a252f48) -- diagnose
   separately before it muddies a future round's gate.

### Round 5 (2026-07-04, at 7533264) -- byte-string map-key inserts + traversal closure

Re-emitted the baseline at HEAD 7533264 (031 N-Quads / 032 builder thaw did
not perturb the gql alloc profile -- ranking held): IC3 4.25M, Q6 3.52M,
Q12 3.09M, CR1 2.95M, Q18 1.98M. Per-query `-memprofile` pinpointed the
alloc sites; every fix is a general engine primitive keyed on runtime value
kind / generic operations, not a query recognizer (see the no-overfitting
goal now in CLAUDE.md).

Four wins, all parity-gated at 49/49 MATCH:

1. **inMembership.resultFor scratch (IC3)** -- the memHash IN-probe called
   `memKey(nil, v)` per row, allocating a fresh key each time. Reuse an
   `inMembership.probe` byte buffer (probes run single-threaded per
   execution; compiled trees are built per-run in execPlan, never shared
   concurrently). IC3 4,252,190 -> 34,502 allocs (99.2%), 62.3M -> 10.0M b.
2. **distinctSet entity-id fast path (Q6)** -- `count(DISTINCT v)` dedup kept
   a `map[string]struct{}` whose per-insert `string(key)` allocated for every
   new distinct value. Replaced the agg seen-set with node/rel `map[uint32]`
   sets (the Rust engine's entity-id fast path) + a byte-string fallback for
   other kinds. Q6 3,519,653 -> 37,545 allocs (98.9%), and bytes 330M -> 109M
   (the compact u32 key also halves bytes vs the old string key). Dropped the
   earlier value.CmpKey attempt (32-byte comparable key raised bytes to
   560M) in favor of the u32 sets -- no public API addition, no version bump.
3. **cSubquery packNodeKey memo (Q18)** -- correlated EXISTS/COUNT memo keyed
   on `string(AppendKey(...))`, allocating per distinct correlated tuple.
   Added a `memoI map[uint64]int` fast path that packs <=2 node-id slots into
   a uint64 (Q18 correlates on two Person nodes); byte-string memo retained
   for other shapes. Q18 1,980,274 -> 531,741 allocs (73%), 190M -> 125M b.
   Remainder is per-group freshGroup / per-distinct-value adds (inherent).
4. **Snapshot.AppendNeighborsMatch direct CSR walk (Q12)** -- the batch seam
   method ranged over the `NeighborsMatch` iter.Seq, whose returned closure
   heap-escapes the yield across the package boundary (neighborsYield is too
   large to inline). Added a core append method that walks the CSR ranges
   directly (no yield closure) and delegated the seam to it. Q12 3,091,197 ->
   1,933,645 allocs (37%). Q12's remaining leader is exec.varReach's per-call
   BFS state (shape-cache candidate -- next round).

Then three more wins in the same round (2026-07-05), all parity-gated 49/49:

5. **list-scope inner environment cache (CR1)** -- extendRow rebuilt a
   comprehension/quantifier/reduce inner scope (full slot-map copy + two
   slices) on every per-row evaluation. Cache the lexically-invariant slot
   map + idx per AST node on the Ctx (alongside subqShapes) and refill only
   the row buffer; a tree AST never evaluates a node re-entrantly, so one
   buffer per node is safe. CR1 2,951,398 -> 2,000,491 allocs, 366M -> 254M b.
   General across every list comp / all-any-none-single / reduce.
6. **varReach dedup-BFS scratch reuse (Q12)** -- the distinct-reachable BFS
   allocated two dedup maps + frontier/next/neighbor slices per outer row.
   Hang a reachScratch on the stage's genScratch (already row-loop-reused),
   clear + reset per call, double-buffer the frontier via a swap. Q12
   1,933,645 -> 62,772 allocs (96.8%), 20.5M -> 5.5M b.
7. **decorate-sort-undecorate ORDER BY (IS5)** -- the sort comparator
   re-evaluated each row's ORDER BY key per comparison (O(n log n) row+scope
   allocs). Precompute each row's key vector once into a flat buffer under an
   invariant scope, then compare precomputed keys; shared by the projection
   and aggregate sort paths (retired cmpOrder/orderKey). IS5 412,413 -> 320
   allocs. General across every non-trivial ORDER BY.
8. **sorted-slice semijoin sets (IC5)** -- the bound-target rebind semijoin
   memoized each target's reverse-neighbor set as a roaring bitmap built one
   Add at a time (a container per 64k id range). The set is only probed for
   membership, so hold it as a sorted id slice (one alloc/target) + binary
   search, filled via the now-closure-free AppendNeighborsByType (delegates
   to the direct core CSR walk). IC5 625,042 -> 27,177 allocs (95.7%),
   30M -> 17M b.

9. **cross-segment monotonic pushdown into the TRAIL walk (CR1)** -- CR1's
   `LET ts = [r IN rels(p) | r.createTime] FILTER all(i IN range(0,size(ts)-2)
   WHERE ts[i] > ts[i+1])` expresses a strictly-decreasing constraint on the
   trail's edge times. The engine already had projection-derived mono
   pushdown (derivedMonoShape handles both `<`/`>`) + the varWalk mono
   pruning, but it only fired when the LET projection and the FILTER shared a
   segment. GQL lowers LET and FILTER as separate clauses, so CR1's filter
   (Segment 1) sat downstream of the var-expand + ts-definition (Segment 0),
   out of the same-segment pushdown's reach -- the plan showed no MonoHop and
   ran `all(...)` post-enumeration on all ~93 paths. Added
   pushCrossSegmentMono: after planPart assembles the segments, walk back
   from a later segment's mono-shaped filter conjunct to the segment defining
   the alias as a rels-comprehension (unbroken passthrough required between),
   push MonoHopSpec onto its var-expand. CPU profile confirmed the lever was
   time, not allocs: interpreted per-path eval (eval.Eval 31% + ceval 34% +
   evalBinary/evalIndex) of the comprehension + quantifier, once per
   enumerated path. Now the walk prunes non-monotonic trails during
   enumeration. CR1 203.7 -> 81.8 ms (2.5x), 2.00M -> 833k allocs, 254M ->
   93M b. General across any LET-projected monotonic-trail filter; new test
   TestCrossSegmentMonoPushdown.
   - Keep-vs-drop RESOLVED (drop is unconditionally safe; guard consumed).
     Worked out the drop-safe condition rigorously + empirically. The walk
     emits a path only when every hop key coerces to an int (AsInt ok) and
     strictly continues the order -- exactly the set all()/range accepts --
     so the emitted set is a SUBSET of the filtered set (W subseteq
     filter-accepted) and the post-filter removes nothing. The null-key
     worry was a misread: an unset i64 rel prop reads as 0 (a present int,
     AsInt ok), not null, so there is no vacuous-null divergence for int
     keys; a genuinely non-int (e.g. string) key only makes the walk emit
     FEWER paths (over-prune), which the guard cannot restore anyway, so it
     is orthogonal to drop-vs-keep. Empirically: TestMonoPushdownFires
     confirms the pushdown engages the walk pruning, and
     TestCrossSegmentMonoDropCorrectness pins the engine result against the
     plain filter semantics on decreasing / non-decreasing / vacuous-length
     trails. pushCrossSegmentMono now consumes the conjunct. CR1 81.8 ->
     49.5 ms (203.7 baseline: 4.1x), 833k -> 653k allocs. (The same-segment
     pushDerivedMonoPred still keeps its guard -- unchanged, its own
     established test; it is redundant there too and could be dropped as a
     follow-up.)
   - CORRECTION (033, 2026-07-05): the "unconditionally safe" rationale was
     wrong on two counts. "Unset i64 reads as 0" holds only for DENSE
     columns (>= 80% fill); sparse/rank columns return absent -> Null, so
     the AsInt-only walk over-pruned rows the filter keeps (vacuous 1-hop,
     violation-count nulls) -- and over-pruning is precisely what a kept
     guard cannot restore, so the same-segment "guard keeps it correct"
     belief was equally wrong. Non-int keys (float) emptied results the
     same way. Fixed by making the walk mirror the filter exactly: hop
     pairs compare via three-valued value.Compare, MonoHopSpec.NullsPass
     carries the per-shape null semantics, min-0 never gets a spec, and
     both push forms consume the conjunct. CR1 unchanged (~same ms).
     Details in tasks/033; the dense-0-vs-sparse-null engine wart is
     tasks/041.

gql suite: 17,455,156 -> 1,646,547 allocs (90.6% total this round). Remaining
top offenders: CR1 653k (surviving-path ts materialization + named-path
reconstruction -- inherent), Q18 532k (inherent per-group / per-distinct-value
adds), then everything <63k. Public core additions this round:
Snapshot.AppendNeighborsMatch -- a version bump is warranted per the tagging
policy when these land as a release (Eve opted to batch the tag for later).

### Round 6 (2026-07-10) -- manifest grew 49 -> 89; new leaders profiled fresh

Baseline re-emitted at 2fa25f4 (89 queries; suite 49.5M allocs). The ranking
shifted completely: the never-profiled BI additions dominate -- Q17 36.1M
allocs / 3.0GB, Q4 6.5M / 1.39GB, Q10 2.75M / 407MB; then CR1 653k, SPB a5
652k / a25 566k, Q18 532k. Per-query -memprofile attribution:

- Q17: ~90% under the WHERE conjunct -- value.Map 22.7M + value.Duration
  22.1M: `duration({hours: 4})` rebuilt map + value per candidate row (the
  map literal compiles to cSlow, so the existing foldFunc never saw a cLit
  arg).
- Q4: ~all aggregator -- freshGroup 6.9M + update 4.1M + finalize 2.2M
  (~2.3M groups x ~6 allocs each).
- Q10: path machinery -- pathRelPositions 1.66M + pathFromParents 1.15M +
  RelationshipsMatched iter closures 737k + value.Path 468k (the round-5
  "convert if a profile surfaces it" note fired).

Fixes, all parity-gated:

1. **Compile-time constant folding** (compile.go) -- constExpr walks the
   AST for row/graph-independence (list-scope iteration vars tracked as
   bound; params count as const since New resolves them per execution;
   every FuncOp is deterministic; startNode/endNode fail resolution);
   row-independent interpreter-backed subtrees (map literals included)
   compile to their literal, and the structural nodes (unary/binary/list/
   in/isnull) fold bottom-up through cLit inputs via one ceval. Q17
   36,057,391 -> 5,803 allocs (3.0GB -> 10.9MB), 34.4s -> 15.2s.
2. **Lazy range() in list scopes** (eval/lists.go) -- listSource gives the
   all/any/none/single, reduce, and comprehension forms a lazily iterated
   integer sequence for range(a,b[,s]) sources under exactly applyRange's
   semantics (pinned by TestLazyRangeListScope); no boxed list per
   evaluation. No bench mover (CR1's quantifier was consumed by the
   round-5 mono pushdown), but general for any surviving range idiom.
3. **Flat chunked aggregator slabs + packed group index** (aggregate.go) --
   aggGroup/freshGroup replaced by per-aggregator chunk slabs (4096
   groups/chunk: no growth-copy bytes, at most one partial chunk waste)
   for keys/states/seen, and group keys packing into a uint64 (single
   entity id or 62-bit int, or an entity pair below 2^30 -- 2-bit shape
   tags keep the key spaces disjoint, mirroring AppendKey) route through
   a map[uint64]int whose inserts allocate nothing; unpackable keys keep
   the byte-string map. Q4 6,506,071 -> 1,109,983 allocs (-83%); Q18
   531,807 -> 271,265 (-49%; the round-5 "inherent per-group" remainder
   halved). Interleaved 3-run A/B on Q4 (loaded machine): 29.5s new vs
   30.6s baseline -- wall-time neutral-to-better; the earlier single-run
   "regression" was load noise. The first flat-slab cut grew Q4's total
   bytes 1.39GB -> 2.25GB via slab-doubling copies; the chunked form
   brought it to 1.18GB, below baseline.

Round 6 gate: 89/89 MATCH (verify-only + emission at 7d30ecc), 45s
FuzzQuery clean. Suite: 49,475,614 -> 6,519,136 allocs (86.8%); bytes
7.68GB -> 4.44GB. Aggregation-heavy SPB swept along: a25 566k -> 329k,
a5 652k -> 218k. Q17 wall time 34.4s -> 15.2s. Timings this round are
otherwise untrustworthy (machine under concurrent load; alloc counts are
the signal). No public API change; no tag.

Remaining leaders: Q10 2.74M (shortest-path stage: per-pair parent map,
RelationshipsMatched iter.Seq closures, pathRelPositions/pathFromParents
slices -- scratch-reuse + batch-seam conversion, the round-5 leftover),
SPB a13 339k (row-proportional), CR1 624k, BI/Q19 210k.

### Round 6b (2026-07-10) -- shortest-path stage scratch + batch seam

Q10's chunk, per the attribution above. All general machinery in
shortest.go, no query shapes:

- spScratch per runSPStage: the walk's visited set + double-buffered
  frontier (reachScratch precedent), the single-path parent map, the
  all-shortest dist map, and batch neighbor buffers -- cleared per call,
  never escaping (the spTree memo's retained parent maps still allocate
  fresh).
- appendHopNeighbors: hop iteration through AppendNeighborsMatched /
  AppendRelationshipsMatched with in-place predicate compaction -- no
  per-hop iter.Seq yield closures. pathRelPositions reads each pair's
  candidates the same way. The recursive all-shortest enumeratePaths
  keeps the iterator form deliberately: its nesting cannot share one
  buffer (documented in code).
- pathFromParents: count-then-fill-backward, one exactly-sized
  allocation, no reverse pass.
- shortestPath / spTree.pathTo return nodesRels by value (found flag),
  removing the per-path box.

Q10 2,750,466 -> 822,769 allocs (-70%), 407MB -> 343MB; remainder is
retained path materialization (nodes/rels slices + value.Path, the same
cost the Rust side pays) + interpreted-filter residue. Q19/IC14
unchanged (weighted Dijkstra path, untouched -- its own scratch pass is
a candidate if a profile ever ranks it). Gate 89/89 MATCH. Suite now
~4.6M allocs (from 49.5M at this round's baseline).

### Next round (open)

- CR1 last mile (optional, ~440k): `all(x IN range(a,b) WHERE p)` materializes
  the range list only to iterate it -- a lazy range source inside the
  quantifier/comprehension evaluators would avoid applyRange's list + per-elem
  boxing. General for any range-in-quantifier idiom, but a targeted eval
  special-case; weigh against the no-overfitting bar before landing.
- native: remaining ~2.3M suite allocs are result-row [][]any boxing (proportional to
  emitted rows, same materialization cost the rust side pays) and kernel-local maps
  (BI/Q18 199k, SPB a13 1.0M for 336k rows). Only worth touching with a general
  mechanism (e.g. arena rows), not per-kernel tweaks.
- shortest.go filteredNeighbors still uses the iter.Seq seam (low volume; convert if a
  profile ever surfaces it).
- TestPlanCacheConcurrent flake (Len()=0, pre-existing at a252f48) -- has not
  recurred under -race this round, but still wants its own diagnosis.
  RESOLVED (2026-07-10): PlanCache.insert released a replaced L1 entry's
  hold BEFORE taking the new one; when both held the same plan (two
  racing first-runs of one query string), the refcount transiently hit
  zero and deref deleted the byTemplate entry while its L1 holders lived
  on -- Len()==0 with a fully working cache. Reproduced deterministically
  at -count=300 -race on a loaded box; incref-before-deref fixes it;
  1000x -race clean after.

## Final outcome (2026-07-10, closing)

Filed 2026-07-03; six-plus rounds over a week. Where it landed, against
the same-commit dc7de70 sweep:

- **Native**: 85,342,501 -> 2,267,750 allocs (97.3%). The remainder is
  measured and characterized: SPB output materialization at 2-4
  allocs/row (the same cost the Rust counting allocator reports) plus
  two kernels' local scratch maps -- split out as tasks/060 (low
  priority, by this task's own only-general-mechanisms rule).
- **gql**: 87,056,543 at the round-3 baseline -> low single-digit
  millions across the grown 89-query manifest, via streaming push
  pipelines, subquery shape caches, byte-string key scratch, entity-id
  distinct/memo/group fast paths, chunked flat aggregator slabs,
  compile-time const folding, fused comparisons, scratch-reused
  shortest paths, streaming top-k, and segment-boundary streaming (the
  later rounds under tasks/055/056, this task's step-3/4 continuation).
- **Step 3's CPU pass** became tasks/055 (ratio table published:
  worst cell 143.6x -> 42.2x, four cells under 10x) -- profile-first
  throughout, every change shape-keyed, parity gate green at every
  round (49/49 grown to 89/89).
- Side finds fixed along the way: the PlanCache refcount flake, the
  dense-0-vs-sparse-null wart (041), the mono-pushdown correction
  (033), the typedPairFor mutex regression (caught by sweep-validation
  discipline before publication).

Remaining ideas all have homes: tasks/060 (native residue), the
scan-level batch filter (noted in 055's close), rustychickpeas 250/255
(cross-repo divergences). Closing.
