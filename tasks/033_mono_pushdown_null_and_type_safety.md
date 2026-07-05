# 033: mono pushdown -- null/type safety and the dropped guard

Code review of the 028 round-5 commits (0d1da7b, f8a2f38) confirmed three
correctness bugs in the cross-segment monotonic pushdown. All three trace to
the same root: the walk-level prune and the original filter disagree on
missing/non-int keys, and the guard that would have masked the divergence was
dropped. The 028 note "drop unconditionally safe" is wrong -- the "unset i64
key reads as 0" premise is refuted by the column code (column.go:50-52 dense
and column.go:159-163 sparse both return ok=false for unset positions).

## Bug 1 -- missing key drops rows the filter keeps

`monoFilter.value` !ok prunes the hop (varexpand.go:263), including the first
hop where there is no previous value to compare. But the filter semantics keep
such paths in two recognized shapes:

- `all(i IN range(0,size(ts)-2) ...)` over a 1-hop path is all() over an
  empty range = vacuously true.
- violation-count shape `size([i ... WHERE ts[i-1]<=ts[i]])=0`: a null
  comparison is not counted as a violation, so the filter keeps the path at
  ANY length with a missing key.

With the conjunct consumed (mono.go:311-316) nothing re-admits the rows:
silent under-emission on sparse rel-property columns.

## Bug 2 -- non-int mono key returns empty results

`monoFilter.value` coerces via AsInt only (value.go:157 -- KindInt only);
mono.go pushes MonoHop on AST key name with no column-type gate. A FLOAT
(or string/temporal) key makes every hop !ok -> every path pruned -> empty
result set, where plain filter semantics compare natively and return rows.

## Bug 3 -- Min==0 loses the filter entirely

mono.go gates only on Max!=nil (mono.go:48, :367, :407), so MonoHop attaches
to `{0,n}` quantifiers; exec routes Min==0 to varReach (varexpand.go:75) which
never consults op.MonoHop. With the conjunct consumed, no monotonic filtering
runs at all -> extra wrong rows.

## Related divergence (review confirmed)

Same-segment `pushDerivedMonoPred` KEEPS the conjunct as a guard
(mono.go:64-88) while `pushCrossSegmentMono` drops it (mono.go:315). Given
bugs 1-2 make the walk under-emit (guard cannot re-admit rows), restoring the
guard alone does NOT fix bug 1; it does mask bug 3.

## Fix direction

- Gate the pushdown on provable safety: only push (and only drop the guard)
  when the key's column is i64 and the walk semantics provably match the
  recognized filter shape on missing keys; otherwise keep the plain filter.
- Make the walk semantics match the filter shape: first hop with !ok must not
  be pruned for the vacuous-all() shape; decide per-shape whether a missing
  key is a violation, or simply refuse pushdown for columns that are sparse
  / non-i64.
- Exclude Min==0 from MonoHop attachment (or teach varReach the filter),
  keeping the conjunct in that case.
- Add parity/regression tests: sparse mono key column, float mono key,
  `{0,n}` quantifier with mono filter, 1-hop trail with missing key -- each
  compared against the unpushed filter path.
