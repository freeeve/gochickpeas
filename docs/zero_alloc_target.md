# Zero-alloc target: a catalog of Go allocation-reduction strategies

General techniques for driving a Go hot path's allocations/op toward
zero. Nothing here is specific to graph engines--each entry is a
language-level pattern, stated generally, with this repo's proof case
attached as a worked example (the campaign that produced this file took a
benchmark battery from ~158k allocations to under 8k). Add new entries as
techniques land; cite the commit that proved the win.

## First, the measurement discipline

1. **Profile before theorizing.** Set `runtime.MemProfileRate = 1` and
   write an allocs profile; `go tool pprof -alloc_objects -list <func>`
   attributes to the line. Code-reading theories are wrong often enough
   to be expensive--in one pass here, two theories were wrong and the
   first "fix" *regressed* allocations 35% (1b861e0's commit message
   records the stepwise correction).
2. **Measure the steady state.** Count Mallocs over one *warm* run
   (`runtime.MemStats` delta) so one-time lazy initialization doesn't
   masquerade as per-op cost. A raw profile window mixes cold and warm --
   sanity-check attributions against the warm number.
3. **A/B each step independently.** Allocation counts are deterministic
   even on a noisy machine (wall time is not). A two-part fix can hide a
   regression in one half.
4. **Guard behavior with a result-identity oracle.** Fast paths must be
   provably output-identical to the general path; run the oracle after
   every step.
5. **Census the structure; don't derive its cost from the code.** Make a
   memo/cache report its own numbers at teardown (keys, stored entries,
   hits) -- an allocation total says HOW MANY, only the structure's
   numbers say WHICH, and they surface facts no reading produces (the
   rustychickpeas tail-memo pass made four confident wrong attributions
   from the build loop before `comps == keys` ended it in one step;
   their 4916766/b7544f3). Our counter oracles (semijoinSetBuilds,
   colAggFired, topkPayloadBuilds, interpPinned) are this discipline in
   test form. Corollary from the same pass: a futility/eviction
   heuristic must measure YIELD, not hits -- a memo with one entry per
   key looks productive by hit count while amortizing nothing. (Audited
   2026-08-08: no hit-based futility heuristic exists in this engine.)
6. **Census the PATH before the payload: a zero delta means you changed
   unreachable code.** When a measured fix produces exactly no change,
   the narrow reading is that the workload runs a different branch --
   one print per candidate path settles it in a run (rustychickpeas
   3d4aba7 landed a real sort fix in a sort the queries never executed;
   their fifth wrong code-shape attribution in that arc, our
   mutation-of-an-untraveled-branch in task 254 is the same failure
   from the test side).
7. **Audit a differential battery PER FIXTURE against the injection, not
   as a whole.** A battery "fails under injection" while individual
   fixtures pass vacuously, and two distinct mechanisms produce that
   (one found in each engine on the same day): (a) the retained set is
   stable under the fault -- small bounds, ties that keep the same top;
   fixed by widening the bound until the fault intersects the retained
   set (our wrapper-key LIMIT 4 fixture, 5ca0a6c); (b) the fixture's
   key shares a period with the injection stride, so the fault lands
   entirely outside the retained set at ANY bound -- widening cannot
   fix it, the key or the stride must change (rustychickpeas' (p*7+m)%3
   key against drop-every-third, their 7c85bca; the tell was every top
   row in one residue class). Listing which fixtures diverge under the
   injection catches both without knowing which one you have; a fixture
   that cannot fail guards nothing. Write the expected diverge set into
   the test header so adding a fixture forces a re-run. Third mode
   (rustychickpeas 069140c, found porting #10): the guarded branch is
   UNREACHABLE BY CONSTRUCTION -- their tie fixture's ORDER BY tuple
   contained the distinct column, so two tuples could never tie and the
   tie-handling branch never ran at any bound. Modes (a) and (b) assume
   the path executes; before trusting a fixture, ask what must be EQUAL
   (or otherwise coincide) for the branch to execute and check the
   fixture actually produces that.
8. **A ladder LOCALIZES; it does not ATTRIBUTE.** Removing a clause and
   watching the number drop names a rung, not a mechanism: the rung may
   move the number by CHANGING THE PLAN rather than by doing the work
   its name suggests. rustychickpeas read a remove-the-DISTINCT delta
   (388k allocs) as "the DISTINCT's cost"; three matched-shape
   synthetics missing it by 178x sent them to EXPLAIN, which showed the
   two rungs were different plans entirely (a decorrelation path vs a
   group-join). Confirm a ladder attribution with a matched-shape
   synthetic or a profile of the mechanism itself -- both engines' Q6
   carry the same surface query with different dominant mechanisms
   (their pivot decorrelation, our per-group distinct sets), so
   Q6-lever ports in either direction are category errors. Refinement
   from the same arc (their 489/f75c4a0): CARDINALITY PARITY IS NOT
   WORKLOAD PARITY when the structure is id-range-sensitive -- a
   synthetic matching Q6's group/pivot/row/element counts still
   modelled it 6.7x under, because sparse-set cost scales with how
   widely the collected ids are SCATTERED and the fixture's narrow id
   space collapsed every set to a dense container. A matched-shape
   synthetic must match the id-space structure (width, scatter) of the
   real workload, or it measures its own layout. Corollary they also
   paid for: byte-identical A/B legs are a bug in the EXPERIMENT until
   proven otherwise (their decorrelation flag did not cover the pivot
   recognizer) -- our engagement-counter + dead-switch guard pairs, one
   per mechanism, are the test-form defense.
9. **Upper-bound a layout change with a throwaway hack before pricing
   it from bytes.** rustychickpeas priced their slab growth churn at
   memcpy bandwidth (~4% of wall clock) and nearly declined the fix; a
   two-line deliberately-unshippable pre-reserve hack (never grows,
   over-allocates wildly, exists only to bound the win) measured
   1.23-1.38x -- five times the estimate -- and the shipped block
   layout landed at ~1.17x (their 291). Byte math misses allocator and
   cache effects; the hack costs minutes and cannot mislead because it
   is never committed. Corollary already in our practice: the
   aggregator slabs are the chunked block layout from the start
   (chunkGroups blocks sized in groups -- the unit callers slice by --
   so a group's window never straddles a boundary), which is why the
   churn their fix removed never existed here.

## Where Go allocates, and what to do about it

### 1. Built-in maps in hot loops

`map[K]V` allocates a bucket array per doubling plus overflow buckets as
it fills--and `m[string(b)] = v` on a `[]byte` scratch forces a fresh
immutable string per **insert** (the compiler elides the copy only for
lookups). Cures, roughly in order of effort:

- **Reuse with `clear(m)`**: buckets persist, so a map whose high-water
  is bounded costs nothing warm. The cheapest fix when the map's
  *lifetime* is the problem rather than its shape.
- **Flat open-addressing tables** (this repo: `internal/flatset`): one
  backing slice, one allocation per doubling, no overflow buckets, no
  per-insert boxing. Byte keys intern into a shared arena probed by
  (offset, length)--N distinct keys cost O(log N) allocations, not N
  strings. Here: DISTINCT/GROUP BY structures fell 99%+ (6f40b16,
  0975879, ff0ba38, 5ec635f).
- **Map-of-maps → packed pair keys**: `m[a][b]bool` allocates an inner
  map per outer key. Pack `(a<<32|b)` into one sorted `[]uint64` when you
  need per-`a` iteration (binary-search the span) plus a flat set when
  you need O(1) membership. Here: 11,568 → 156 on the worst case
  (456b86e).
- **Inline small-N fast paths**: when most instances hold a handful of
  entries, a fixed-size inline array probed linearly (spilling to the
  table on overflow) makes the common case zero-heap. Here: 4×24-byte
  inline slots on the byte set; an 8-entry id array on the entity set
  (177a127).
- **Dense slabs over a known index**: if the key universe is small and
  enumerable up front, index it once (sorted keys, position = dense
  index) and count into `[]int64` slabs merged by vector add.
- **The probe path owns nothing.** A cache read more often than written
  must build its key in reused scratch and probe without allocating;
  owning (copying) the key is an insert-path cost only. The `m[string(b)]`
  read elision covers exactly the single-byte-slice-key case -- a struct
  key, a `Sprintf`-assembled key, or probing `map[[N]byte]V` through a
  slice all allocate per probe, invisibly (rustychickpeas measured 1.00
  alloc/row from exactly this on their tail memo, 4916766). Audited here
  2026-08-08: the hot probes are already in this shape -- hash-join
  `tables[string(keyBuf)]` (reused scratch, copy paid on insert only),
  `constCalls` keyed by AST pointer, `ByteMap`/`U64Map` probing raw
  bytes/ints through flat tables.
- **Map-of-bucket-slices → intrusive chains behind a flat probe table**:
  `m[k] = append(m[k], idx)` pays a first-append allocation per distinct
  key plus a growth ladder per bucket. Give each stored row a `next
  int32` link, keep per-key `heads/tails` in parallel slabs behind a
  flat table (`U64Map`/`ByteMap` → chain slot), and append at the tail
  --insertion order survives, per-row cost drops to zero. Pairs
  naturally with packing the rows' own payload slices into append-only
  table slabs handed out as capped sub-slices (a later slab growth
  copies the backing but retained sub-slices stay valid). Here: the
  hash-join build table and group-join side table, Q17 -79% / Q12 -85%
  (058361a).

### 2. Scratch allocated per call / per iteration

- **Hoist + reset** is the master pattern: `clear(map)`, `slice[:0]`,
  walk a queue by head index instead of `q = q[1:]` (reslicing loses the
  backing for reuse). Ownership lives with the caller because generic
  code cannot reset an opaque `T`. Here: a BFS kernel went 694 → 2
  (ab29532).
- **Tiny-slice arguments**: `f([]T{x})` allocates per call. Keep a
  `[1]T` field on the (single-owner, non-concurrent) struct and pass
  `buf[:]` (76f64a2).
- **`append(s[:0], make([]T, n)...)` is the zero-alloc grow-and-zero
  reset**, not an allocation: the compiler's extend-slice optimization
  zeroes in place without materializing the temp (verified by
  measurement on genMatches' entry buffers -- <0.5 allocs/row over
  2,000 per-row calls, pinned by
  TestGenMatchesEntryScratchDoesNotAllocPerRow). The Rust analog
  (fresh Vec per call) IS a per-call allocation, so cross-engine ports
  of this shape must not assume the trap transfers -- nor "fix" the Go
  idiom into an equivalent by hand.
- **Per-node scratch on compiled trees**: a tree evaluated sequentially
  and never shared across goroutines can carry its own argument buffer --
  but only after auditing that no callee *retains* the slice (20fb310).
  When the structure IS shared (cached plans, shared ASTs), put the
  scratch on the per-execution context instead--as a stack of frames if
  calls nest (387cd8a).
- **`sync.Pool` for state reached from many call paths**: a
  point-to-point search rebuilt two maps and two heaps per call; pooling
  the scratch (concurrent-safe, GC-relief valve built in) took a caller
  from 62 allocations to 4 (cb5a804). Prefer explicit borrowed scratch
  when there's a single owner; pools when ownership is diffuse.

### 3. Parallel workers and their accumulators

- **Accumulate per worker, not per load-balancing chunk.** Work-splitting
  APIs often oversplit (e.g. 4× workers) for balance; if each chunk
  builds a heavy accumulator (a map, a slab), the oversplit multiplies it
  for nothing when per-item cost is near-uniform. Contiguous in-order
  ranges keep reduce order deterministic. Here: `parallel.Fold`'s rework
  dropped four call sites 55-79% at once (577ef04).
- **Borrowed accumulators across calls**: let the caller own pre-seeded
  per-worker accumulators and reset them between calls (`FoldInto`), so a
  warm fold allocates goroutine machinery only (2dced1f: 501 → 61).
- **Mind goroutine machinery itself**: one goroutine per chunk is ~2
  allocations each; W long-lived range workers beat 4W chunk goroutines.
- **The queue-vs-static-ranges dispatch verdict is RUNTIME-dependent --
  measure on YOUR scheduler before changing it.** The Rust sibling
  measured plain one-range-per-worker beating its pooled-worker chunk
  queue on uniform work (their f9b0cdb, 20 vs 27 ms); in Go on the SAME
  12P+4E box the A/B inverts -- the 4x-oversplit atomic queue beat
  static goroutine ranges in all three regimes (uniform 267 vs 321 us,
  scattered skew 0.97 vs 1.2 ms, clustered skew 2.4 vs 9.2 ms;
  internal/parallel BenchmarkFor*). Same silicon, opposite verdicts, so
  the split keys on the runtime, not the cores (their re-run falsified
  this entry's first draft, which blamed P/E heterogeneity). Candidate
  mechanisms, unproven: OS thread migration policies differ from
  goroutine scheduling within a range, and workload memory-density
  shifts the P/E throughput gap. The alloc-side rule above holds in
  BOTH runtimes (accumulators per worker, never per chunk); only the
  dispatch shape is scheduler-dependent. Known tension: `Fold`/
  `FoldInto` use static contiguous ranges for their in-order reduce
  contract -- revisit only with sweep-refereed timings if a fold ever
  dominates a kernel's wall.

### 4. Iterator closures (`iter.Seq`) on hot seams

A returned iterator closure allocates per call--fine at API granularity,
death by a thousand cuts per-element inside a search loop. Provide (and
prefer) **append-into-caller-buffer batch variants** for the hot seams
(4875a0a, 8790c13). Caveat: a batch pays the full sweep where an
early-exit iterator pays half on average--if the loop usually breaks
early on a large input, measure both (the 1b861e0 stepwise record shows
batching alone regressing until the chunk granularity was fixed with it).

### 5. Boxing rows/results per element

- **Typed structs until the last moment**: collect candidates as plain
  structs, sort/truncate typed, and materialize only survivors into the
  interface-ish output form. Boxing every candidate before a top-k
  truncation dominated several hot paths here (−98% class).
- **Flat backing for row-of-slices output**: one `n*width` backing with
  full-capacity subslices (`cells[i*w : i*w+w : i*w+w]`) turns n row
  allocations into two.
- **Presize appends** when the length (or an upper bound) is known --
  append growth is O(log n) reallocations you don't need to pay.

### 6. Recomputing constants per element

- **Memoize deterministic constant-argument calls per execution** --
  keyed on the call site, stored on per-execution state (never on shared
  structures). A constant timestamp parse ran once per visited row until
  memoized: −97% (3039f6b).
- **Scan-first fast paths**: check cheaply whether the expensive general
  path is needed. An all-ASCII string's substring is a zero-copy slice
  (Go strings share backing); only non-ASCII pays the rune conversion
  (177a127).

### 7. Bulk construction over incremental insert

Compressed/structured containers (bitmaps, sorted indexes) often pay
per-insert container management. Collect keys into a plain slice, sort,
construct once (the `nodeset.Of` fix inside 1b861e0: −63% on its caller).

### 8. Fold projection boundaries into aggregates at plan level

The cheapest materialization is the one the plan never emits: a pure 1:1
projection boundary (LET, RETURN...NEXT) directly before an aggregating
projection contributes nothing but a materialized intermediate table --
substitute its aliases into the aggregate and drop the segment. Two
reachability traps made the pass fire on almost nothing until fixed
(mirroring rustychickpeas): star projections were excluded even though
LET lowers to `*` + computed aliases (starred columns pass through and
contribute no substitution entry), and the terminal RETURN sits outside
the clause list, so the most common shape of all -- a binding right
before the query's own aggregate -- was structurally unreachable. Both
arms iterate to a fixpoint so chained LETs fold completely. Fires on
generic structure only; declines DISTINCT boundaries (would change
count(*)) and two-level aggregation (ff0d73f + the star/fixpoint
follow-up).

### 9. Stream finalized aggregate groups through the bounded top-k

When the aggregating boundary ITSELF carries ORDER BY + LIMIT, finalize
does not need to materialize one row per group and sort: each group
finalizes into a reused stride-wide scratch, its ORDER BY key vector
evaluates from the scratch, and only rows the bounded heap would admit
are copied out (wouldAccept before copy, the task 257 gate pattern). At
most bound rows exist instead of nGroups, and the sort's full-width key
decoration never exists. Two sizing lessons from the A/B rounds: (a) a
bounded sink must right-size its arena chunks (rowArena.chunkValues) --
a fixed 16K-value chunk for a LIMIT 100 sink REGRESSED bytes on cells
with few groups until sized to the bound; (b) presize the heap's
parallel arrays to the bound (capped) or append regrowth shows up as a
constant ~20 allocs/run. Value-driven engagement gate, not a plan gate:
nGroups > bound, known only at finalize. Many-groups + small LIMIT
(expand join, group per person, LIMIT 10, sf1): 4.29 MB -> 2.13 MB per
run (-50%); cells whose ordered aggregate has few groups are neutral by
the gate.

### 10. Ordered DISTINCT+LIMIT over an aggregate is a grouped argmin

[aggregating boundary] -> [identity ORDER BY boundary] -> [DISTINCT
column-subset + LIMIT] does not need the sort at all: DISTINCT keeps
each tuple's FIRST row under the total order, so each tuple's effective
position is the MINIMUM of its rows' key vectors, and the answer is a
bounded selection over those minima. Stream the aggregate's finalized
groups (the #9 scratch) through a per-tuple argmin, then top-k the
minima -- the full group-row materialization, the sort decoration, and
the sort itself never exist. Two costs to keep flat: back tuple values
with a rowArena, and index tuple identity through the flat maps
(packGroupKey1 -> U64Map for a packable single value, ByteMap
otherwise) -- a Go map with string keys costs ~2 allocs per distinct
tuple, which on BI/Q4 was a 44x allocation regression (189k/run) until
the flat maps replaced it. Q4: 617.7 -> 339.8 MB/run (-45% on top of
the #9+passthrough state; 1,132.9 MB two days of levers ago), allocs
+84 over baseline (amortized slab growth). Argmin tie keeps the earlier
group sequence, matching the sort's index tiebreak, so results are
byte-identical to sort-then-dedup-then-truncate. Port caveat
(rustychickpeas 069140c): the BOUNDING argument (minima only improve,
threshold only tightens, eviction safe) is universal, but the eviction
COMPARATOR is not -- it must be the exact total order the engine's
unbounded path finally sorts by. Ours is (keys, arrival sequence);
theirs breaks ties by key values, and rank-only eviction there admits a
wrong set whenever more than bound tuples share a rank.

### 11. Geometric first chunks for slab accumulators

A slab-chunked accumulator (the aggregator's per-group windows) that
allocates fixed full-size chunks pays the whole first chunk on its first
element: a 218-group aggregate allocated 0.63 MB of slab capacity per
run against ~20 KB of live use (BI Q8, measured by the base-diff
protocol above). Grow the first chunks geometrically (128, 512, 2048,
then the uniform 4096) and keep the O(1) index->window mapping with a
three-comparison prefix switch; a large aggregate pays at most the
extra seams, a small one allocates proportionally to what it groups.
Q8's appendGroup: 643 KB -> 100 KB per run. Wall was FLAT there -- the
query is subquery-eval-bound -- so this entry is an allocation win with
the honest label, not a latency claim; the same measurement showed the
madvise CPU share was cross-thread scavenger work that best-of wall
never saw. Proving commit: the aggregate slab-tier change (task 205
round 5).

## Anti-patterns and honest labels

- **Don't move cost--label it.** Reusing scratch across calls is a real
  reduction; moving *computation* into an untimed setup phase changes
  what the number means. If a change relocates work, the commit must say
  so.
- **Floors are hypotheses.** Every "genuine floor" declared during this
  campaign fell when re-challenged with a fresh profile (the
  "membership-probe floors" fell 85-99% the same day they were
  questioned). Declare a floor only alongside the profile line proving
  the residual is structural--and expect to be wrong.
- **No workload recognizers.** A change that helps only because the code
  knows *which* input is running is overfitting. The test: would an
  unseen input of the same shape benefit?
