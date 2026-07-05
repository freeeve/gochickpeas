# 040: hoist per-relKey invariants out of the WriteNQuads inner loop

In WriteNQuads (nquads.go) the innermost loop `for _, rk := range relKeys`
(nquads.go:141) runs per (rel x relKey) and recomputes two per-relKey
invariants each iteration:

- the predicate IRI `nqPropPrefix + nqEscape(rk.name)` (nquads.go:145) --
  an allocating concat+escape;
- the column lookup `g.relColumns[rk.key]` (nquads.go:142).

For M rels and K rel-prop columns that is M*K redundant string allocations
and map lookups where only K distinct values exist. Precompute both into the
relColKey struct when relKeys is built (nquads.go:102-108). The writer is
already buffered (nqFlushSize), so this is the remaining hot-loop cost.

Verify the byte-identical round-trip test still passes.

## Resolution (2026-07-05)

Hoisted all three per-iteration invariants out of the writer loops:

- relColKey now carries the precomputed predicate Term and the Column
  (the specced M*K concat+escape allocations and map lookups drop to K).
- The rel-type predicate Term is cached per type atom (was resolved and
  escaped per rel).
- The label object Term is cached per label (was escaped per node x label).

Measured on FinBench SF10 (9M rels, warm, WriteNQuads -> io.Discard):
158.6s -> 85.8s, 87.7M -> 61.8M allocs, 1936 -> 1169 MB allocated.
Output byte-identity pinned by SHA-256 over the full SPB export
(4.2M-resource graph): old and new writers hash identically. NQuads
round-trip tests green.
