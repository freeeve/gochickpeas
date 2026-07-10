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
