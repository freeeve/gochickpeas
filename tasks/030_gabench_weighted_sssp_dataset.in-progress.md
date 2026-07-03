# 030 — gate GA SSSP: run gabench on a weighted dataset (kgs)

**Status:** pending
**Filed by:** rustychickpeas-ldbc session 2026-07-03 (uncommitted, per the ldbc repo's cross-repo
boundary rule — a concurrent gochickpeas session should adopt/renumber freely).
**Cross-ref:** ldbc `tasks/268` (close the last unverified go cells), ldbc `tasks/263` (native parity
manifest — GA sub-item: "provision the GA datasets + reference outputs to the Go side and agree a
per-vertex compare, float epsilon for PR/LCC/SSSP").

## Why

On the ldbc viz, the two `gochickpeas (go)` **GA SSSP** dots render as `○ unverified (no parity
check)`. Diagnosis: `gabench` has only ever been run with `-datasets wiki-Talk`, and **SSSP is
unvalidatable on wiki-Talk** — wiki-Talk is unweighted (`graph.wiki-Talk.directed = true`, edge rows
are `src dst` with no weight) and its official algorithm set is `bfs, cdlp, lcc, pr, wcc` (**no
sssp**), so `datasets.ldbcouncil.org` ships no `wiki-Talk-SSSP` reference. `gabench` correctly emits
SSSP unvalidated in that case (`cmd/gabench/main.go:169` leaves `parity = ""` when `reference()` finds
no `<name>-SSSP` file). rcp does the identical thing — its only gated SSSP is on a weighted graph.

**Nothing is broken in the Go SSSP path.** It is fully wired already:
- `GASSSP` reads the `weight` rel property (`internal/ldbc/ga_algos.go:54`).
- `SSSPSource` is parsed from `graph.<name>.sssp.source-vertex` (`ga_test.go` covers it).
- `GACheckEpsilon(…, 1e-6)` gates SSSP with correct infinity semantics
  (`internal/ldbc/ga_validate.go:64` — both-infinite same-sign passes, mixed finite/infinite fails),
  and `TestGASSSPWeightedAndUnreachable` already exercises the weighted + unreachable case.

The only thing missing is a **weighted dataset that officially supports SSSP** to run it against.

## Dataset — already provisioned by the ldbc side

The ldbc session downloaded and extracted **`kgs`** into the shared, gitignored
`~/rustychickpeas-ldbc/data/graphalytics/` (the same dir the Go side already reads for wiki-Talk):

    kgs.v  kgs.e  kgs.properties  kgs-{BFS,CDLP,LCC,PR,SSSP,WCC}   (+ kgs.tar.zst)

`kgs.properties` confirms it fits SSSP exactly:

    graph.kgs.meta.vertices   = 832247
    graph.kgs.meta.edges      = 17891698
    graph.kgs.directed        = false
    graph.kgs.edge-properties.names = weight        # WEIGHTED edges (3rd col in kgs.e)
    graph.kgs.algorithms      = bfs, cdlp, lcc, pr, sssp, wcc   # SSSP in the set
    graph.kgs.sssp.weight-property = weight
    graph.kgs.sssp.source-vertex   = 239044

`kgs.e` rows are `src dst weight` (e.g. `0 1 1`, `0 2 0.142857`); `kgs-SSSP` rows are
`<vertex-id> <distance>` floats — the epsilon oracle `GACheckEpsilon` already consumes.

## What to do

1. **Run `gabench` on `kgs`** (adds a second GA dataset; all six algos gate, SSSP included):

       go run ./cmd/gabench \
         -data ~/rustychickpeas-ldbc/data/graphalytics \
         -datasets kgs \
         -out bench-out/emitted_gochickpeas.jsonl

   Expected console: all six PASS — BFS/CDLP exact, WCC relabel-invariant, PR/LCC/SSSP within 1e-6.
   Each emits `parity: "MATCH"`. The `kgs` SSSP cell is the gated SSSP number the viz was missing.
   (kgs is 832k nodes / 17.9M undirected edges — heavier than wiki-Talk for PR/CDLP/LCC but well
   within range; run `-datasets kgs` alone if you want to isolate it from a wiki-Talk sweep.)

2. **(Recommended) Make `gabench` respect the per-graph algorithm set** so the residual
   wiki-Talk SSSP `unverified` dot stops being emitted at all. Right now the algo loop runs all six
   regardless; have it skip any algorithm not listed in `graph.<name>.algorithms` (already parsed into
   the dataset properties). Then wiki-Talk emits only its five valid algos, kgs emits all six, and
   there is no meaningless SSSP-on-an-unweighted-graph cell anywhere. This matches how the LDBC spec
   scopes algorithms per dataset. (The ldbc user's chosen resolution was "provision a weighted
   dataset"; this step 2 is the honest cleanup that removes the leftover wiki-Talk dot — do it unless
   you'd rather keep wiki-Talk SSSP as an explicit unvalidated point.)

## Done when

- `bench-out/emitted_gochickpeas.jsonl` carries GA `kgs` records with `parity: "MATCH"` for all six
  algorithms, SSSP included, stamped at the current go HEAD.
- (If step 2 done) no `wiki-Talk` SSSP record is emitted going forward.
- Ping the ldbc side to re-run `viz/import_gochickpeas.sh` — the gated SSSP MATCH then replaces the
  `○ unverified` dot on the site. No ldbc code change is needed (`extract.py` normalizes `"MATCH"`
  the same for every engine).

## Note — LOAD is NOT part of this task

The four `gochickpeas (go)` **LOAD** cells that also read `unverified` are a **stale ldbc import**, not
a Go gap: your `bench-out/emitted_gochickpeas.jsonl` already has the diff-gated LOAD records at
`21bb3c2` (task 029, all MATCH, 14:20), which supersede the pre-gate `48acb90` rows (10:27). The
ldbc side just needs to re-import; handled there. No Go action for LOAD.
