# 056 -- stream rows across NEXT segment boundaries (055 follow-up)

Opened 2026-07-10, scoped from tasks/055 round-6 findings.

## Problem

The push pipeline (tasks/028 round 4) streams within a segment, but a
NEXT boundary still materializes the full inter-segment row set: each
segment's terminal sink finalizes into [][]value.Value and the next
segment seeds from it. IC9 is the canonical victim -- its ORDER BY +
LIMIT 20 lives in the FINAL segment, so the middle segment's ~500k
message rows materialize (523MB of its remaining bytes profile) even
though the streaming top-k (055 round 6) then discards all but 20.

## Direction

Chain segments as sinks: when segment N+1's stages are per-row
convertible from segment N's output (the same condition runSegment's
own push pipeline already tests stage-by-stage), N's terminal sink
becomes N+1's row source and rows flow through the boundary without
materializing. Segments whose head requires the full input (an
aggregate seeding a scan, DISTINCT-consuming ORDER BY over carried
rows) keep the materialization -- exactly the same degeneration rule
the intra-segment pipeline uses.

Watch items, from the round-4 notes: batch-constant hoisting is derived
statically per segment (const = unbound by any stage in the segment and
identical across seeded inputs) -- a streamed boundary must not break
that derivation; PROFILE counters accumulate per stage across pushes;
OPTIONAL re-emit and PathBind wrap per row and should compose
unchanged.

## Verify

- Parity gate 89/89 (rowhash-pinned ordering catches boundary
  reordering).
- Alloc profiles on the multi-segment ORDER+LIMIT shapes: IC9's bytes
  should collapse (523MB -> tens of MB); IC2/Q2 similar shapes.
- The intra-segment streaming tests (tasks/028 round 4) as the
  semantics reference.

## Outcome (2026-07-10)

Implemented as scoped: runBranch groups each maximal run of segments
whose interior boundaries are streamable (streamableBoundary: no
aggregation, DISTINCT, ORDER BY, pagination, or aggregate wrappers) and
runSegmentRun chains them -- every non-final segment's terminal becomes
a passthroughSink that projects the output columns, applies the
boundary WHERE per row, and re-seeds the next chain's width buffer; only
the final segment's terminal retains state, and its post-WHERE runs as
before. The first segment keeps the materialized-inputs batch-constant
analysis; streamed-into segments compile with nothing batch-constant
(perf-only distinction -- HoistConstIn untriggered is value-identical,
and cInCarried still fires per epoch). PROFILE keeps the per-segment
materializing path, so its counters are unchanged. Each chain gets its
own rel-uniqueness env, preserving per-MATCH scope.

Measured: IC9 boundary bytes 523.5MB -> 4.4MB (-99.2%), IC2 68.2MB ->
1.4MB; allocs 1,034 -> 678 and 385 -> 313; single-segment Q2 bit-
identical and untouched. Gate 89/89 MATCH, full suite green.

The 45s FuzzQuery insurance surfaced a dual-path divergence on
MATCH(A:!A)-[A]-()RETURN(0) -- verified PRE-EXISTING by stashing this
change (still fails), filed as tasks/058 with the repro; the failing
seed was deliberately not committed to the corpus.
