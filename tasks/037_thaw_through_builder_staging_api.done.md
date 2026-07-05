# 037: route thaw through the builder's staging invariants

Review of 032's thaw path (thaw.go) found it bypasses the builder API and
duplicates its body, so builder invariants now live in two places:

- `thawRels` (thaw.go:81-86) replicates AddRel's body verbatim -- rels/
  relTypes append, degOut++/degIn++, knownNodes.Add -- without calling
  AddRel (builder.go:180-187), skipping its capacity/limit checks and
  relIndex maintenance. `b.relIndex` is never touched; masked only because
  findRelIndex lazily rebuilds when nil (builder.go:300-301) and a thawed
  builder starts nil. Any future staging invariant added to AddRel silently
  diverges in the thaw path.
- `thawNodeColumn`/`thawRelColumn` assign column pair slices directly
  (thaw.go:194 `b.nodeColI64[key]=pairs`, :253 `b.relColStr[key]=pairs`)
  instead of going through SetPropByKey (builder.go:256).

Direction: add a builder-level bulk staging entry point ("stage pre-typed
pairs" / "stage rel with known endpoints") that owns the invariants, and have
thaw call it. Keep the fuzz oracle (fuzz_removal_test.go) as the gate;
extend it to assert relIndex correctness after thaw+AddRel interleavings.

## Resolution (2026-07-05)

- AddRel's body extracted into `addRelTyped(u, v, typeID)` -- the one
  rel-staging core owning capacity/count ceilings, degrees, known
  endpoints, and lazy relIndex maintenance. AddRel interns and delegates;
  thawRels calls it per restaged rel (byte-identical round trip pinned by
  the existing TestThawRoundTripByteIdentical).
- Bulk column staging entries `setNodeColumnPairs`/`setRelColumnPairs`
  (builder.go, generic over the four pair types) own the per-pair
  invariants: node side ensures capacity + registers known nodes; rel side
  checks every id is a live staged rel index. thawNodeColumn/thawRelColumn
  build the typed pairs and install through them.
- Thaw-side failures of these calls are impossible for a consistent
  snapshot (pre-sized builder); `thawMust` panics loudly if an engine bug
  ever violates that.
- builder.go split: pre-finalization read probes (NodeCount/RelCount/Prop/
  NodesWithProperty/NodeLabels/NeighborIDs/ResolveString) moved to
  builder_read.go for the file-size norm.
- New TestThawRelIndexInterleaving pins first-match addressing across
  thaw -> index build -> AddRel -> duplicate-parallel interleavings.
- Fuzz found my 036 oracle assertion too strict across the mid-run thaw:
  a dense column materializes staged zero pairs the model treats as
  absent, so removed=true/model-absent is legitimate; the check is now
  one-directional (model-had => must report removed) with the fuzzer's
  counterexample kept as a regression seed
  (testdata/fuzz/FuzzRemovalOracle/f7bdbdfaf19523e2).
