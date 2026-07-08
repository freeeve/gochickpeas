# 053: Lazy atoms -- topology-only ParseLazy should not load the atom table

Filed from libcat (its tasks/167: SKOS vocabulary index served from an RCPG
in blob storage, range-fetched). Measured there on real LCSH (513,497
concepts / 626,488 rels, RCPG 66MB):

- `ParseLazy(TopologyOnlyParseOptions())` fetches 60.7MB of the 66MB file
  because the **atoms section (47MB) is loaded even topology-only**; decoded
  it dominates the resident set (~175MB of ~180MB total).
- The rels CSR itself is 13.7MB and traversals over it are ns-scale --
  topology-only residency could be ~20MB if atoms were lazy.

Ask: let a topology-only (or explicitly atom-lazy) parse skip fetching the
atom section, resolving atom ids on demand -- e.g. block-lazy atoms behind a
SectionFetch, the same shape as roaringrange RRTI's blocked dictionary
(resident router, range-fetched blocks). This is the first consumer-shaped
slice of the deliberately deferred CSR-skeleton/working-set machinery
(rcpg/lazy.go's package comment).

Consumer pattern, for context: label/URI *values* are already served by
sidecars (RRTI typeahead, RRIL uri->node), so the graph path only needs atom
resolution for rel-type filtering and occasional display -- a small hot set.

## Disposition (2026-07-08)

Landed in rcpg, within the frozen v1 format:

- `ParseOptions.SkipAtoms` + `SkeletonParseOptions()`: skeleton loads
  (CSR + indexes, no columns, no atom table) never fetch or decode the
  atoms section, in both `ParseWith` and `ParseLazy`/`PlanSections`.
  On the LCSH numbers that is the 60.7MB fetch -> ~13.7MB and the ~175MB
  decoded table gone from the resident set.
- `AtomReader`: on-demand atom resolution over a `SectionFetch`, RRTI
  shape -- resident block router (one u64 per 1024 atoms), range-fetched
  blocks, FIFO-bounded decoded-block cache. Last-duplicate-section-wins
  and bounds semantics mirror the eager decoder; UTF-8 checks run per
  decoded block. Structural corruption surfaces as ErrCorrupt, fuzzed.

Known limit, inherent to v1: the atoms section has no in-file offset
index, so the router is discovered by scanning length prefixes forward.
First resolution past the scanned frontier transfers the section up to
that atom once (boundaries kept, bytes discarded -- residency stays
bounded). Rel-type atoms intern late in typical build order, so the first
type-atom lookup pays a one-time full-section scan. Killing that needs a
format-level block index: proposed as rustychickpeas tasks/253 (optional
section id 7, opt-in writer, corpus-lockstep), with a follow-up here to
consume it once FORMAT.md lands.

Verified: rcpg suite + 20s FuzzAtomReader clean, full module green,
parity gate 89/89 MATCH, consumer-seat sample drive (skeleton parse ->
CSR traversal -> type-atom resolution -> parity sweep -> malformed-input
probes) through the public export.
