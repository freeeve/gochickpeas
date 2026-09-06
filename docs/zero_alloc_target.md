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
   key looks productive by hit count while amortizing nothing. (Re-audited
   2026-09-06 after the memo additions -- matcher/compiled-expr/decor/
   packed-key-bank: the only futility gate is decorrelation's
   decorBuilds>=64 self-disable, which is builds-based but NOT the
   sibling's wide-tail defect [their 396] -- decor stores exactly ONE
   table per anchor key, so builds IS the cost unit, and every other
   memo here stores one artifact per key too. Measured decorOff to fire
   ZERO times across the corpus [no query exceeds 64 distinct anchors],
   so it is dormant rather than proven-good [a zero-count's two causes,
   item further below]. No key-gated wide-tail memo exists to leak the
   sibling's 58k-103k wasted completions.)
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
10. **Price a reuse/pooling fork by histogram replay before building
    either side.** Capture one warm execution's allocation SIZE
    HISTOGRAM (MemProfileRate=1 profile gives it), replay just those
    allocations in a standalone loop, and compare that wall time to
    the query's: reuse cannot save more than the allocator time it
    removes, so the replay upper-bounds the win in an afternoon,
    without implementing pooling or a session API. The bound is
    generous in three ways that all push the true figure lower:
    pooled memory still gets cleared and re-touched, the replay skips
    the real run's interleaved frees, and not every captured
    allocation is per-execution scratch. Cross-engine validation: the
    Rust sibling's histogram replay bounded their aggregator-scratch
    reuse at 1.02-1.40% of wall where our env-gated sync.Pool
    prototype (research/agg-slab-pool) measured ~1-2% -- two methods,
    two allocators, same verdict. The prototype is tighter and proves
    the mechanism works; the replay is the cheap first pass for a fork
    you suspect will come back "not worth it".
11. **Match the differential's comparison to the property under test:
    when ORDERING is the risk, compare rows IN ORDER.** A multiset or
    sorted comparison of two legs is vacuously green against any
    reordering bug -- the group-join tail differential compared result
    MAPS for months while the ordering claim it implicitly guarded went
    unpinned (fixed with an in-order tie-cutting differential; both
    engines had the same blind spot, Rust catalogued it as their item
    37). The counter-example that keeps this from reading as
    distrust-of-sorting: for a genuinely UNORDERED result contract,
    the multiset comparison is the correct oracle, and forcing order
    would over-constrain -- failing legitimate reorderings the
    contract permits. The question to ask per differential is "what
    property does this comparison actually pin?", not "sorted or
    unsorted".
12. **When the load gate will not clear, measure a BOUND with
    min-of-N.** (Rust sibling's item 38, from a probe their gate
    refused three times.) Two moves rescue a measurement on a box that
    never quiets: (a) report the MINIMUM of N interleaved reps per leg
    -- interference only ever adds time, so the minimum is nearest the
    uncontended cost, and the cross-leg RATIO agreeing across
    contaminated runs is the consistency check when neither absolute
    is citable; (b) ask for a bound, not a value -- contention
    inflates both legs, so a contaminated delta upper-bounds the true
    delta under proportional slowdown, which is enough to DECLINE an
    optimization without ever publishing an untrustworthy timing.
    Declining on a bound is publishable; the timing itself is not.
    LIMIT (Rust's 39, found the hard way): min-of-N has a resolving
    power of roughly the box's BETWEEN-run drift (~20% here) -- it
    removes upward noise within a run and does nothing across runs. It
    settled a 2x ratio and manufactured five phantom losses on a 3-7%
    effect. For small effects, run the whole A/B twice and gate each
    cell on DIRECTION agreement (same side of the band both passes;
    else UNRESOLVED) -- and gate on side, not spread: two passes at
    1.37x/1.31x agree ("helps, magnitude noisy"), and a spread gate
    wrongly discards them. A/Bs also need liveness assertions: a
    harness that measures nothing must FAIL, not print an
    empty-tabled "0 regressions". And skip the alloc-substitution
    escape for per-row-work effects: a reproducible 1.3x timing win
    can carry an allocation delta of exactly zero.

8. **A floor claim is a claim with a date and an instrument attached.**
   Re-price standing floor/diagnosis notes when the MEASUREMENT METHOD
   improves, not only when the code changes -- an attribution that
   predates the current instrument is unverified by it. CR1's "fill
   floor" note stood false for weeks because it predated the rate-1
   delta-profile method; the re-price found 86.6k of 87.3k allocs/op in
   two per-row allocations and removed 99% of them (411b7ed). Same-day
   second instance: Q17's "diffuse" note hid a flipped cache template
   re-planning per hit (71cf3ac, 13 queries). Convention shared with
   rustychickpeas (their re-pricing-trigger framing; our finding).

9. **A cross-engine (or cross-branch) corroboration inherits the weaker
   instrument's validity.** Two independently-measured numbers landing
   1.5ms apart read as confirmation and are exactly as seductive as
   they are uninformative when one leg is confounded -- the sibling
   engine's ic9 "convergence" was recorded as an established floor for
   hours before their leg was retracted (branch based 201 commits
   behind main, so their A/B measured drift plus the change). Two
   rules: an A/B's legs must share a merge-base with current main,
   checked BEFORE the run (`git rev-list --count $(git merge-base A
   B)..A` -- a nonzero count on either leg voids the comparison;
   the sibling's one-liner, adopted verbatim -- the sharp part being
   WHY existing gates miss it: a CLEAN timing verdict, a quiet-box
   lock, non-overlapping legs, and byte-identical rows all validate
   the RUN, and none of them inspect what the binaries CONTAIN); and a conclusion drawn from someone else's
   number carries that number's instrument note, so a retraction
   upstream can be traced and unwound downstream (ours was, same-day
   -- tasks 376/377/610/611). Confirmation has a HALF-LIFE, not a QED:
   two cross-engine claims died on the second look this week after
   surviving the first (the 610 convergence off a 201-commits-behind
   branch; the "AggState tax is Rust-specific" agreement, confirmed by
   both engines then falsified by an unsafe.Sizeof) -- "the other
   engine checked and agrees" raises a claim's bar without closing it,
   so a confirmed cross-engine fact is strong evidence to re-derive
   before building ON, not a settled premise. Three corollaries from the sibling's
   full write-up (their 44/45): a parked branch's status label is a
   claim with a date on it -- price a revival by BUILDING in a
   worktree, not by reading the label; a port of a result-identical
   optimization cannot be validated by the identity oracle (a prune
   wired to a stale threshold still returns correct rows, just
   without pruning) -- re-verify ENGAGEMENT on the new base before
   timing; and an engagement counter on the wrong side of a
   `continue`-based reject reads as absence -- ours count the offered
   side with rejects on a separate counter, and any new counter must
   state which side it samples.

10. **A control is a claim about the control, and needs its own
    instrument reading.** The sibling engine's flip-cost probe used a
    second literal of the same query shape as its quiet control; the
    control was itself flipped, both legs were routed, and the
    allocation column alone read "flipping is cheaper" -- a confident,
    backwards conclusion from a clean-looking number, caught only by
    the engagement counter sitting NEXT to it (their 382). Same family:
    our DESC top-k case legitimately rejects zero candidates, which is
    also what a deleted mechanism produces -- so the ASC twin asserts a
    PREDICTED reject count alongside the engagement count (their 381
    flag, adopted). A number without its engagement reading is not yet
    a measurement of the thing named. And a cross-engine (or
    cross-component) counter comparison is valid only when both
    counters sample the SAME SEAM: two engines' "ic11 candidates"
    differed 17x -- one counted a distinct-terminal fill, the other a
    top-k sink -- while the true populations matched to the unit once
    the seams aligned (their 384/our 616); query name and dataset are
    not enough to make two counters comparable. And before publishing
    a DERIVED figure, name the denominator's scope and check it matches
    the numerator's: a per-query total divided by output rows printed
    as "fan-in" was wrong by four orders of magnitude the moment a
    query had more than one aggregate stage -- and invisible on every
    query where the stage count happened to be one (the sibling's 386,
    their item 48, adopted).

11. **When a fix has an exact discrete signature, pin the signature,
    not the resource it happened to consume.** The sibling's flip-memo
    pin is "0 plan builds across 5 warm hits" (reads 5 with the memo
    disabled -- fails pre-fix for the right reason), NOT an allocation
    bound: the two states the fix separates ("planned again" vs
    "planned again but cheaply") differ by exactly one integer on the
    build counter and are indistinguishable inside a 2x-headroom alloc
    bound that would drift into noise and silently stop meaning
    anything (their 383). Allocation counts are good motivation and
    poor pins. Corollary for our own counters: a package-level counter
    asserted exactly is a claim that its readers run sequentially --
    the second test to engage it breaks the FIRST test's name; the
    constraint is now documented at the counter sites.

12. **Before believing an operator-ORDERING claim, count the WORK you
    think is misplaced, not the rows flowing past it.** A rendered plan
    shows a Filter after three expands whose conjuncts reference only
    the first expand's variables -- a textbook missed pushdown worth
    ~90%% of the walk. It was already pushed: the render position is
    display order, and hand-writing the pushed form gave IDENTICAL
    predicate-evaluation counts (the sibling's q11, their item 49). The
    failure is not a wrong number -- every row count printed was
    correct -- it is a causal story ("the filter runs where it is
    drawn") laid over correct counts, which is harder to catch because
    there is nothing to distrust in the data. Two forms doing the same
    work agree on a work-counter exactly; find that counter (predicate
    evals, candidate-fill sweeps) before trusting the tree's order.
    Adjacent to item 5's "census the structure" and the engagement-
    census rule: the render tree is not an instrument, it is a display.
13. **Before pricing a data structure, OPEN it -- and confirm the cost
    model you are applying is THIS structure's, not a same-named
    sibling's.** The sibling priced a futility gate as watching the
    wrong quantity (keys vs completions) and put the waste at a third of
    a query; the numbers were real but came from a DIFFERENT mechanism
    -- their chain memo stores a Vec per completion (wide tail), while
    the tail memo they measured stores into a per-KEY arena, so its
    key-count gate is exactly right and the measured ceiling was 13%,
    mostly on an instance worth keeping (their 397, item 50). Two
    minutes reading the storage type would have shown it. Third
    boundary-crossing cost model of the week: item 12's render-order-as-
    execution-order, item 48's per-instance ratio over a per-query
    total, and this storage-model-from-a-same-named-sibling. The general
    failure: a cost model is valid only within the structure it was
    derived from, and carrying it to a same-named or same-shaped neighbor
    produces real arithmetic about the wrong thing. Local relevance: this
    engine now has four similarly-named memos (MatcherMemo, compile.Memo,
    the decor shared store, the packed-key bank) -- price each against
    ITS own storage, never by name-analogy.
14. **Ship a runtime A/B hatch on any lever you might later measure at
    single-digit percent -- even one that looks obviously
    one-directional.** Two prebuilt binaries answer "is there a
    difference" but cannot ATTRIBUTE a small one to the source change,
    because the binaries differ in more than the source: link order and
    function alignment alone produce ~1.5% between independently built
    artifacts, no source cause needed. Disjoint ABBA ranges rule out
    run-to-run noise, not a systematic layout difference. Only a
    same-binary toggle in ONE process separates the two. The sibling's
    q4 aggregate levers went out without a hatch and their gated ABBA
    landed at -1.5% disjoint -- unattributable between real gain and
    layout, and by the time the box went quiet the cheap option was
    gone (their 400). Our own q4 wall NULL had the same flaw from the
    other direction: two-binary ABBA, legs interleaving, which is even
    weaker evidence of no-effect than their disjoint 1.5%. Both agree
    the CELL does not move, which is all that was at stake -- but
    neither measurement can price the effect, because neither lever was
    built A/B-able in one process. (Our packed-key half HAS the hatch
    via disablePackedKeys; the slim-aggState half does not, having
    removed the fat path it would toggle against.) The hatch costs two
    lines at build time and is unavailable at measure time; add it
    prospectively to any lever whose wall effect you might one day want
    to attribute.

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

- **Count maps (`m[k]++`) flatten the same way**: an `Inc` on the flat
  u64 table (mint at 1, else `val++`) replaces the Go map's bucket
  churn with one slice allocation per doubling. Here: the decorrelated
  COUNT side tables, IC10 917 -> 400 allocs/op (374f69c).

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

### 12. Cross-run scratch: GC-lifetime pools vs bounded strong banks

Applicability test first: this class exists only for scratch dimensioned
by the ID SPACE (dense per-id arrays, bitmaps). Structures dimensioned
by the REACHED SET (sparse maps growing to what a bounded walk touches)
have per-use costs proportional to the work and nothing worth pooling --
"you pay for the id space, or you pay for the reached set" decides
whether the whole entry applies before any measurement (rustychickpeas'
structural decline of the pool port, their 372; expires for them if
their search ever goes dense).

`sync.Pool` reuse works when the scratch's consumers run hot relative
to collection cadence (the shortest-path search arrays, 2e5616d). It is
structurally unfit when each USE allocates enough to force a collection
between uses: the pool's two-GC lifetime frees the contents before the
next use arrives -- measured 2% hit rate on aggregation distinct-set
tables at ~79MB allocated per run (the instrumented failure is
preserved on research/agg-rec-pool). The fix is a bounded,
strongly-referenced bank (N items, max held bytes each, mutex on
checkout/checkin -- two touches per use, not per element), with a
harvest pass at every terminal returning each structure's FINAL backing
array, which growth-time recycling alone never recovers (0358fc9: Q9
2974 -> 282, Q4 3744 -> 1082, five more riders). Two portable details:
check out lazily so non-users never touch the store (an early version
leaked the full bank to non-users who never returned it), and harvest
at ALL emitting terminals, not just the common one.

### 13. Don't materialize what is only measured

A value built per row so that a later expression can take its LENGTH is
an integer wearing a container: when plan-level analysis proves every
read of a path/list variable is `length(x)`/`size(x)`, bind the count
and skip the construction entirely (shortest-path form: 2e5616d, Q10
8801 -> 936; var-expand rel-list form: 411b7ed, CR1 87302 -> 758). The
elision pass must run AFTER the reduction passes that collapse derived
reads (comprehension-length, path-alias rewrites) to the bare size
read, or it is sound but fireless; a live path bind is a hidden
consumer no expression scan sees and must decline. Two-phase trial
rewrite (all sites convert or none do) with an engagement counter, so
differential tests cannot pass vacuously on a silent decline.

### 14. An escape hatch that routes around a cache is a cache-shaped hole

A "detected hazard -> bypass the cache" route re-pays the cached work
per hit, forever, once a key is hazard-marked -- priced as rare when
built, permanent in effect. If the routed work is deterministic per key
(here: sighted planning per verbatim query text against a fixed
snapshot), cache ITS result on the key's entry instead of exempting the
key (71cf3ac: the flip-detection routing re-parsed and re-planned 13 of
89 corpus queries on every hit; headline Q17 1383 -> 863).

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
