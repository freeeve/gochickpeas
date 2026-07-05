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
