# 029 — loadbench correctness diff-gate (parity for LOAD records)

**Status:** pending
**Filed by:** rustychickpeas-ldbc session 2026-07-03 (uncommitted, per the ldbc repo's
cross-repo boundary rule — a concurrent gochickpeas session should adopt/renumber freely).
**Cross-ref:** ldbc `tasks/252` (load throughput bench), ldbc `python/cypher/graph_diff.py`
(the reference implementation) + `python/cypher/emit_loads.py` (the MATCH/DIFF wiring).

## Why

The ldbc viz Loads heatmap (`viz/src/LoadView.tsx`) now shows one block **per engine**, and it
renders a cell as **✗** whenever the LOAD record's `parity` normalizes to `diff`. The rcp side
diff-gates every reloaded graph before its throughput counts (a format that reconstructs a
*different* graph is drawn WRONG, never a silent throughput win). `gochickpeas (go)` LOAD records
currently emit `parity: ""` (no verdict), so the Go loads render as a bare ratio with no correctness
check — the one gap called out in the ldbc caption ("the Go loader is not yet" diff-gated).

The check has to run **inside the Go process** — it's the one holding the reloaded graph in memory —
so this is Go-side work. The ldbc side needs **no** change: emit a real verdict in `parity` and the
✗ appears automatically (ldbc `extract.py` normalizes `"MATCH"` / `"DIFF (...)"` the same for every
engine).

## What to build

### 1. A structural graph-diff (`internal/ldbc/graphdiff.go`)

Mirror `python/cypher/graph_diff.py` — layered cheapest-first, return on first mismatch. Everything
keys off the **external id property** (the dense `NodeID` is assigned per load and differs between
snapshots), so the diff is order- and id-assignment-independent.

```
DiffGraphs(ref, test *chickpeas.Snapshot, opts DiffOpts) (ok bool, detail string)
```

Layers (all primitives already exist on `Snapshot`):
1. **totals** — `NodeCount()` + `RelCount()`.
2. **per-label / per-type** — node count per label (`NodesWithLabel(lbl).Len()`), rel count per type
   (`RelCountByType()` — direct, no Cypher needed unlike the rcp side).
3. **sampled props** — for K nodes per label ordered by external id, every listed property matches
   (`Prop(node, key)`); catches value-level corruption a count can't see. Order the sample by the
   external id property, not `NodeID`.
4. **full (opt)** — the complete external-id set per label. O(nodes) identity check.

`detail` is `"MATCH"` or a `"<layer>: <ref> vs <test>"` string naming the first divergence (same
shape as the rust side: `node_count: A vs B`, `label X: A vs B`, `rel R: A vs B`,
`props X: sample of N rows differs`, `id-set X: a ref-only, b test-only`).

### 2. Wire it into `cmd/loadbench/main.go`

- **rcpg cases** (BI / FINBENCH / SPB): rcpg *is* the canonical snapshot, so stamp
  `parity = "MATCH"` (the baseline, exactly as the rust side treats rcpg).
- **nt case** (SPB RDF, the real test): load the SPB **reference** snapshot, `DiffGraphs(ref,
  ntLoaded, ...)`, stamp `parity = "MATCH"` or `"DIFF (<detail>)"`.

Set the record's existing `Parity` field (`internal/ldbc/emit.go` → json `"parity"`); use the exact
`"MATCH"` / `"DIFF (<detail>)"` spelling so ldbc `extract.py` normalizes it like every other engine.

### 3. The reference for the nt diff — the one real design decision

`loadbench` today loads nt from `data/spb/extract/spb-validate.nt` and rcpg from
`{spbDir}/spb_canonical.rcpg`. A diff is only meaningful if those are the **same graph**. Either:
- (a) diff the nt-load against an rcpg **exported from `spb-validate.nt` itself**, or
- (b) confirm `spb-validate.nt` is the canonical SPB source that `spb_canonical.rcpg` was built from.

If they're different sizes today, the gate will (correctly) report DIFF — pick the reference so a
correct nt loader reports MATCH. The SPB canonical label/prop schema for the `nodeProps` layer is the
same one `spbexport` / `spbload` already carry.

## Done when

- `cmd/loadbench` emits `parity` = `MATCH` for the rcpg cases and a real `MATCH`/`DIFF (...)` verdict
  for the SPB nt case.
- Re-sweep + `viz/import_gochickpeas.sh` → the `gochickpeas (go)` block on the ldbc Loads heatmap
  shows a verified ✗ on any format whose reload diverges (and no ✗ when it matches).
