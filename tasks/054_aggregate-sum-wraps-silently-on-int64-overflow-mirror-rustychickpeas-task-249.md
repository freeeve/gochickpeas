# 054 -- aggregate Sum wraps silently on int64 overflow; mirror rustychickpeas task 249

Filed from rustychickpeas on 2026-07-10 (cross-repo ask).

**Severity: MEDIUM correctness.** Rust side fixed in `rustychickpeas` commit `429e0a7`
(`tasks/249_core_aggregate_sum_i64_overflow.done.md`). Eve asked for identical behavior in Go.

## Defect

`aggregate.go:198` declares `Sum int64`, and `aggregate_run.go:299` accumulates it with an
unchecked `e.sum += v` (plus the partial-merge adds at `:324` and `:335`). A group whose total
leaves int64 range wraps. Go has no debug overflow check, so unlike Rust this is silent in
**every** build: summing `2 x math.MaxInt64` yields `-9223372036854775808`, not an error.

## Agreed design (Eve, 2026-07-09)

Semantics must match Rust exactly. Rust chose `AggRow.sum: Option<i64>` with an `i128`
accumulator, specifically so Go can mirror it -- widening the *output* (DuckDB's
`sum(BIGINT) -> HUGEINT`, Postgres' `sum(bigint) -> numeric`) was rejected because Go has no
`int128` and its public API would grow a custom type.

1. **Surface a nullable int64.** `Sum *int64` (nil = the true total is outside int64), or a
   `NullInt64`-shaped struct if that reads better next to the existing fields. `database/sql.
   NullInt64` is the stdlib precedent. `nil` must mean overflow and *only* overflow -- a query
   with no Sum column still reports `0`, not nil, matching Rust's `Some(0)`.

2. **Accumulate in 128 bits.** Accumulator width is independent of the surfaced type, and this is
   the part that matters: the out-of-range verdict must depend on the true total alone, never on
   how the work is partitioned across goroutines. A `[+MaxInt64, +MaxInt64, -MaxInt64, -MaxInt64]`
   group must report `0`, exactly, whatever the chunk boundaries. A naive "checked add, set a
   sticky overflow flag" accumulator does **not** have this property and will produce
   partition-dependent results.

   Use `math/bits.Add64` for a two-word signed accumulator (~15 lines, no dependency); do not pull
   in `big.Int` -- it allocates per group in the hot path. An int64-valued property summed over at
   most 2^32 nodes cannot leave 128-bit range, so the accumulator itself never overflows.

3. **gql maps nil to Null**, matching the engine's overflow policy (Rust `tasks/165`/`236`: no
   per-row error channel, so an unrepresentable total is Null rather than a wrapped integer).
   Check `gql/internal/exec/aggregate.go` for the same unchecked accumulation on the engine's own
   aggregation paths.

## Verify

Port the Rust regression tests (`rustychickpeas-core/src/aggregate/tests.rs`):

- exactness at `math.MaxInt64` / `math.MinInt64`;
- a total one past the boundary reports nil -- both serially and through the parallel merge
  (~20k nodes);
- repeating `[+Max, +Max, Min+1, Min+1]` nets back to exactly `0` (this is the partition-order
  guard; note an *alternating* `+Max/-Max` pattern does not overshoot within a contiguous chunk
  and so would pass even against the buggy code);
- no Sum column still reports `0`, never nil.

Note for whoever ports the "transient excursion" test: in Rust that case is a debug-panic guard,
not a wrong-answer guard, since wrapping addition is exact mod 2^64 whenever the true total fits.
In Go, where nothing panics, it is *purely* a guard against a future checked-add implementation
being partition-order-dependent. Keep it anyway -- it pins the property.
