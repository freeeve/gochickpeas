# 026 — SPB native query kernels (blocked on ldbc-side graph export + manifest rows)

Split out of tasks/025 (all other families shipped at 59/59 parity + GA validated).

## Goal

Port the 30 SPB queries (rustychickpeas-ldbc `src/spb/{q*,a*}.rs`, ~6k lines) as native Go kernels,
gated on per-query rowhash refs, emitting `Family=SPB` rows next to `rcp-native (rust)` — same shape
as the BI/IC/FinBench kernels (tasks/025).

## Blocked on (ldbc-side, their tasks/263)

1. **SPB canonical .rcpg export** — no export exists (`export_gochickpeas.rs` covers SF1 only; the SPB
   graph lives behind their N-Triples loader). The Go runner needs `export/spb_canonical.rcpg` (or
   equivalent) plus the canonical property/rel naming it carries.
2. **Per-query manifest rows** — `python/refs/spb/spb.parity.rust.json` holds all 30 queries' oracle
   rows (kinds: uris / uri_opt / kv), but the native manifest rows (refhash + norm per query) are their
   tasks/263 deliverable; reshape happens in `viz/native_manifest.py`.

## Once unblocked

- Kernels register under `FinBench`-style ids (`SPB/Q1`, `SPB/A17`, …) in `internal/ldbc`; the runner
  (`cmd/ldbcnativebench`) needs no changes beyond the manifest rows.
- Port specs: each `src/spb/<q>.rs` module; params pinned in the parity JSON's `params` block
  (word/topic/entB/category/audience/cwType/date window, geo lat/lon/km).
- Full-text (q8/a20) and geo (q6/a17) queries map onto the core FullTextField / GeoIndex, which the Go
  port already has.

## 2026-07-03 -- unblock path: own N-Triples loader via libcodex (Eve's proposal, surveyed)

Their side hasn't moved (no `export/spb_*.rcpg`, tasks/263 unchanged), but both blockers dissolve
on our side:

1. **Graph**: build our own RDF->property-graph loader instead of waiting for their export. The
   source data is local (`~/rustychickpeas-ldbc/data/spb/extract/spb-validate.nt`, 490MB -- the
   exact file their parity runner loads, TBox included). Parse with
   `github.com/freeeve/libcodex` rdf (streaming NQuads decoder, zero deps, v0.13.0; new dep for
   this repo). The mapping spec is their `src/spb/loader.rs` (376 lines), to mirror exactly:
   - `rdf:type` -> label (IRI local name), plus rdfs:subClassOf transitive closure as extra
     labels (owl:Thing dropped); TBox triples are not instance data.
   - IRI/blank-object predicate -> rel (predicate local name), plus rdfs:subPropertyOf
     super-rels (about/mentions -> tag).
   - Literal-object predicate -> node prop (local name), typed from xsd datatype
     (integer family -> i64, double/float/decimal -> f64, boolean -> bool, else string);
     first literal per (node,key) wins.
   - Every IRI node gets prop `uri` = percent-decoded IRI (cross-engine key). IRIs are
     UCHAR-unescaped (\uXXXX/\UXXXXXXXX) at parse and percent-decoded at intern so
     encoded/raw spellings collapse to one node. Blank vs IRI identity namespaced (B:/I:).
   - libcodex caveats: its parser is lenient (skips malformed lines) -- count and report
     triples/resources/rels/literals like SpbStats, and cross-check against the Rust banner;
     apply UCHAR-unescape/percent-decode ourselves if libcodex leaves them raw.
   New `cmd/spbexport`: .nt -> Builder -> Finalize -> WriteRCPGFile, output kept in this repo
   (their export/ dir is theirs to write). Loader load is one-time; kernels bench off the .rcpg.
2. **Manifest rows**: extend `cmd/nativemanifest` with an SPB arm over the committed
   `python/refs/spb/spb.parity.rust.json` (30 query blocks, kinds: uris/uri_opt/kv/kvx/
   day_count/who_days/pairs) -- per-query rowhash refs + norms, same interim-until-263 status
   as the rest of the manifest. Note their parity rows are FULL result sets with order not
   significant (LIMITs disabled), so norms must sort before hashing.
3. **Params**: all pinned in the parity JSON `params` block, including the data-derived
   `q2_cw` (q1's first row) -- kernels read them from there.

Kernel port unchanged from the original plan: 30 modules from `src/spb/{q*,a*}.rs`, prep/run
split (full-text field + geo index built in untimed prepare), registering `SPB/Q1`..`SPB/A25`.

## 2026-07-03 -- DONE (30/30 MATCH; full suite 89/89)

Shipped on this repo's side only; no ldbc-side deliverable was needed in the end:

1. **Loader** (`internal/ldbc/spbload.go` + `cmd/spbexport`, new dep `github.com/freeeve/libcodex`
   v0.13.0): N-Triples -> property graph mirroring their `src/spb/loader.rs` exactly (subClassOf/
   subPropertyOf forward-chaining, owl:Thing dropped, percent-decoded `uri` key, first-wins typed
   literals). Load-level parity: **947640 resources / 3851898 triples**, equal to the Rust banner.
   Export: `export/spb_canonical.rcpg` (114MB, gitignored; regenerate with
   `go run ./cmd/spbexport -ldbc ~/rustychickpeas-ldbc`).
2. **Manifest arm** (`internal/ldbc/spbref.go`, `cmd/nativemanifest`): 30 `SPB` rows hashed from
   their committed `python/refs/spb/spb.parity.rust.json` per-query blocks (kind `uris` wraps to
   one-cell rows), norm `-` -- rowhash/v1's sorted multiset absorbs the order-free full result
   sets, no sort norm needed. Query ids lowercase (`q1`..`a25`) to join their viz's SPB rows.
3. **Kernels** (`internal/ldbc/native_spb_{a,b,c,d}.go`): all 30, `-verify-only` 30/30 MATCH
   (89/89 with the pre-existing families). Ports preserve the Rust per-rel multiset counting (no
   dedup of parallel about/mentions/tag rels) and the q2/q9/a2 derived-parameter prepare.
   Full-text (a15/a16/a20-a23) and geo (a17) validate FullTextField/GeoIndex against their core.
4. **Bonus (per Eve)**: `cmd/loadbench` emits family=LOAD records (BI/FINBENCH/SPB rcpg + SPB
   nt) in the rcp-native load-emissions shape; new optional Meta fields format/bytes/mb_s/rec_s.

Their tasks/263's authoritative manifest, if it ships, can supersede the interim rows; the
refhashes will not move (same refs, same rowhash/v1).
