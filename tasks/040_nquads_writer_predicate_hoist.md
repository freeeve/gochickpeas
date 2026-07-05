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
