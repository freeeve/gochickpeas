# 059 -- finalize str dense gate counts staged writes not distinct positions; diverges from rustychickpeas 237

Filed from rustychickpeas on 2026-07-10 (cross-repo ask).

**Severity: MEDIUM (RCPG byte-identity divergence + memory blow-up on the str path).**
Rust side fixed in `rustychickpeas` commit `08b1a56` (`tasks/237`).

## Context

`finalize.go` already guards the i64/f64/bool dense selection against duplicate staged writes:

```go
if denseThreshold(len(pairs), span) && coversAllPositions(pairs, span) {
```

and `coversAllPositions`' own comment states the reason -- "a write count can reach span through
duplicates while leaving positions unset". Good.

**But the str path (`finalize.go:165`) does not:**

```go
func denseStrThreshold(pairCount, span int) bool {
	return pairCount >= int(float64(span)*0.8)
}
...
if denseStrThreshold(len(pairs), span) {   // no coversAllPositions
```

`pairCount` is staged writes. Re-setting one string property on a few nodes of a wide column pushes
`pairCount` past `0.8 * span` while the distinct fill stays tiny, so the column stores dense: a
full span-sized `[]uint32` where sparse/rank would hold a handful of entries.

## Why it now matters for conformance

`denseStrThreshold`'s comment says str selection "stays byte-identical with the Rust finalize".
**That is no longer true.** Rust `tasks/237` changed all four of its gates to confirm with a
distinct-position count before committing to dense:

```rust
fn is_dense_fill<V>(pairs: &[(u32, V)], span: usize) -> bool {
    let gate = (span as f64 * 0.8) as usize;
    pairs.len() >= gate && distinct_positions(pairs, span) >= gate
}
```

So for a str column whose staged writes exceed 80% of the span but whose *distinct* positions do
not, Rust now finalizes sparse/rank where Go still finalizes dense -- different `ColumnData` in the
written `.rcpg`, i.e. a byte-identity break. The existing conformance corpus does not exercise this
(no duplicate staged writes), which is why `go_interop` still passes on both sides.

## Fix

Gate the str path on distinct positions, mirroring the other three:

```go
if denseStrThreshold(distinctPositions(pairs), span) {
```

Note the i64/f64/bool rule needs *full* coverage while str needs *80%* coverage, so
`coversAllPositions` cannot be reused verbatim -- it returns `covered == span`. Factor out the
distinct count (the `bitset.New(span)` loop) into a `distinctPositions` helper and have
`coversAllPositions` call it too.

Keep the str `>= 80%` threshold itself: that intentional difference from the i64/f64/bool
full-coverage rule is documented (dense str encodes missing as atom 0 by design) and is not what
this task changes.

## Verify

- A str column with, say, 100 distinct nodes over a 1000 span, each written 9 times (900 staged
  pairs > the 800 gate), must finalize **sparse**, not dense. Mirrors Rust's
  `dense_gate_counts_distinct_positions_not_staged_writes`.
- Re-check `.rcpg` byte-identity against Rust for such a graph. Worth adding one to the shared
  conformance corpus (`gen_conformance` on the Rust side) so the fixture pins the agreement --
  today no corpus graph re-sets a property, so neither side's tests would catch a regression here.

## Related

`rustychickpeas` `tasks/250_core_dense_column_missingness` (the mirror of this repo's 041) is a
*separate*, still-open divergence: Go requires full coverage for i64/f64/bool dense, Rust accepts
80%. Rust's 237 narrows its rule (80% of *distinct* positions) but does not close that gap.
