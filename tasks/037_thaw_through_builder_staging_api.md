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
