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
