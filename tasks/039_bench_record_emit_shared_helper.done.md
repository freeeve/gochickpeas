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

## Resolution (2026-07-05)

internal/ldbc/cell.go now owns the whole shared cell flow:

- VerifyCell(row, cells): ApplyNorm + RowsHash + refhash compare, the
  parity verdict with DIFF detail. (Unifies one prior divergence: gqlbench
  used to SKIP on a RowsHash error while the native bench hard-errored --
  an unhashable cell is a harness bug, so both now hard-error; gql results
  with unconvertible values still SKIP earlier at resultCells/cellOf.)
- TimeSamples(runs, run): the timed-sample loop, sorted ms samples.
- ManifestSF: the FinBench-SF10/else-SF1 rule, previously a verbatim
  comment+rule copy in both emitters.
- NewRecord(RecordSpec, stamp, samples): the emission Record schema every
  bench shares (warm/emitted framing, measured date, percentile block,
  stamp, Port/GoVersion meta defaults) -- gabench and loadbench assemble
  through it too, so a schema change lands in all four emitters at once.
- CellRecord(row, CellIdentity, ...): NewRecord for a manifest row
  (identity strings, SF rule, parity MATCH, graph meta).

Also collapsed NativeRow into ManifestRow (it was ManifestRow minus GQL --
the same drift hazard at the manifest layer); LoadNativeManifest returns
ManifestRows with an empty GQL column.

Acceptance: re-emitted SPB/q1 (native) and FinBench/CR5 (gql) to scratch
and diffed against the historical bench-out records -- schema keys
identical, every non-volatile field identical (timings/stamps/dates
excluded by design). Both gates re-verified MATCH during emission.
