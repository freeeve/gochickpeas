# 041: dense columns cannot represent missing values

Found while fixing 033. Observable property semantics currently depend on
the storage heuristic: a column at >= 80% fill (finalize.go denseThreshold,
note the int() truncation lowers the effective threshold for small spans)
stores dense, and dense i64/f64/bool Get returns (zeroValue, true) for any
in-range unset position (column.go denseI64Col.Get), while the same unset
position in a sparse/rank column reads as absent -> Null. So `r.k` on an
unset property yields 0 on a dense column but NULL on a sparse one, and
`r.k IS NULL`, comparisons, aggregates, and the mono walk (033) all shift
meaning when a dataset crosses the density threshold. Str columns are
exempt (atom 0 means missing); i64/f64/bool are affected.

TestMonoDenseUnsetZeroSemantics (gql/mono_drop_test.go) pins the current
0-read convention -- update it when this lands.

Direction options (pick one):
- Validity bitmap on dense columns (space: span/8 bytes; Get checks the
  bit) -- keeps O(1) dense reads, makes missing representable everywhere.
- Only choose dense at 100% fill (pairCount == span) and route everything
  else to rank-select/sparse -- no format change, small perf cost for the
  80-99% band.

Check the Rust sibling for the same wart; if present, file a mirror task
there per the cross-repo norm.

## Resolution (2026-07-05)

Took the storage-selection option (no format change): i64/f64/bool columns
finalize dense only at FULL coverage -- denseThreshold is now pairCount >=
span plus a coversAllPositions distinct-position check (a write count can
reach span through duplicate writes while leaving positions unset).
Partial columns go to rank-select/sparse, which represent absence exactly,
so `r.k`, IS NULL, comparisons, aggregates, and the mono walk read an
unset property as NULL at any fill ratio. Str keeps the historical >=80%
rule (atom-0-means-missing makes partial dense str lossless by design).

Consequences and verification:

- Deliberate divergence from the Rust finalize's 80% rule for numeric/bool
  (documented in finalize.go's header): the same staged input with a
  partially-filled numeric/bool column no longer finalizes byte-identically
  between engines. Mirror task filed: rustychickpeas tasks/250 (uncommitted,
  cross-repo norm) -- Rust has the identical wart (finalize.rs 0.8 casts,
  dense get() -> Some(zero)).
- Reader unchanged: legacy/Rust-written partial-dense columns still read
  present-zero (that information was destroyed at their write time); thaw
  doc updated accordingly.
- Golden-byte gate: 3 of 4 conformance goldens still byte-identical; the
  "small" case (1-of-2 fill, which the truncating 80% floor stored dense)
  now pins sparse selection + absent reads -- matching the golden's own
  hand-built layout.
- TestRemovalThawInteraction now pins the improvement: a removed rel prop
  at 3-of-4 fill reads absent after refinalize instead of resurrecting as
  dense zero.
- TestMonoDenseUnsetZeroSemantics replaced by
  TestMonoUnsetUniformAcrossFill: the unpadded 5-of-7 graph (historically
  dense via truncation) now behaves identically to the padded sparse one.
- Parity gates green after regenerating the Go-built SPB export:
  89/89 native MATCH, 49/49 gql MATCH. Removal fuzz 45s + query fuzz 30s
  clean; full suite green.
