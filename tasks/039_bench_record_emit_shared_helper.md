# 039: shared bench Record-emit helper in the ldbc package

cmd/gqlbench/main.go:205-244 and cmd/ldbcnativebench/main.go:131-165 are
copy-pasted: identical ApplyNorm/RowsHash/RefHash parity check, verbatim
FinBench-SF10 comment+rule, identical timed-sample loop, and near-identical
ldbc.Record assembly -- differing only in Engine/Shape strings and the
runnable (gql.Run vs krun). gabench/loadbench share the Record tail.

Adding a percentile field or changing the SF convention in one bench but not
the other emits manifests with mismatched schemas, silently corrupting
cross-engine comparisons.

Move the block into the ldbc package (which already owns Record/Percentile/
ApplyNorm/RowsHash/MeasureAllocs): a `RunCell(name string, run func() (rows,
error), ref Ref, opts)` style helper that owns the parity stamp, SF rule,
sampling, and Record assembly; benches pass the runnable and identity
strings. Re-emit a bench cell afterward to confirm byte-identical manifests.
