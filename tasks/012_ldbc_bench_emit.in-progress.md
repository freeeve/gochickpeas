# 012: LDBC kernel cross-check + timing emission for the rustychickpeas viz

Filed by the rustychickpeas-ldbc session (the benchmark suite). gochickpeas is
going in as **two engine columns** on the timings site (rcptest.evefreeman.com),
mirroring the rcp pair: `gochickpeas (go)` (native kernels, the floor) and
`gochickpeas (gql)` (the GQL engine, once it can execute). Ingestion works the
way `ragedb` / `neo4j` do: a foreign runner emits into the suite's shared JSONL
schema, merged by that repo's `viz/extract.py`. This is the go-side half; the
ldbc side merges + syncs the emitted file (their task 257) and authors the
canonical-schema GQL query translations (their task 258).

Everything you need from the Rust side already exists (ldbc task 256):
`$GOCHICKPEAS_SF1_RCPG` -> `sf1.rcpg`, and `testdata/ldbc/sf1_expected.json`
(vendored) -- both at core commit `1315d98`, canonical schema.

Fixture provenance (this task file is the cross-repo channel; there is no
separate coordination doc):

- `sf1.rcpg` lives at `/Users/efreeman/rustychickpeas-ldbc/export/sf1.rcpg`
  (520 MB, gitignored there). Canonical rel types (UPPERCASE, `HAS_CREATOR`
  Message->Person, `KNOWS` stored once) over MIRROR-style property keys
  (`plid`, `flid`, `ms`, `bmon`, `pday` -- authoritative names in the fixture's
  `meta.keys`); node/rel ids are dense internal ids, identical on both engines
  after loading the same file. Serves the NATIVE kernels only.
- Regenerate both files (when core moves or the codec diverges):
  `cargo run --release --bin export_gochickpeas` in the ldbc repo
  (env `GOCHICKPEAS_OUT` / `GOCHICKPEAS_SF1_SRC` / `RCP_CORE_COMMIT`); the
  fixture stamps the core commit it was built at.
- The GQL column uses a DIFFERENT graph:
  `/Users/efreeman/rustychickpeas-ldbc/export/sf1_canonical.rcpg` (532 MB,
  canonical property names like `creationDate`; rebuild instructions in the
  ldbc repo's `gql/README.md`). The manifest's `graph` column carries the path.

## Where we are

`ldbc_test.go` already loads the fixture and cross-checks the **structural**
section (node/rel counts, csr id space, rel-type counts, label cardinalities).
Its `ldbcExpected` struct parses only those 5 fields. Two things are missing:

1. **Kernel correctness** -- the fixture has 6 kernel sections that aren't
   checked yet: `neighbor_groups`, `fold_via_top100`, `common_neighbor_counts`,
   `aggregate.by_birth_month`, `aggregate.by_creation_year`,
   `weighted_shortest_path`. All six Go kernels exist
   (`NeighborGroups`, `FoldVia` + `NeighborVia`, `CommonNeighborCounts`,
   `Aggregate`, `WeightedShortestPath`).
2. **Timing emission** in the suite's schema, parity-gated.

## Deliverable 1 -- kernel cross-check

Extend `ldbcExpected` + `TestLDBCStructural` (or a new `TestLDBCKernels`) to run
each Go kernel on the loaded snapshot and diff against the fixture. Match the
Rust bin's exact shapes (see the fixture `meta.notes` -- it's self-describing):

- `neighbor_groups`: `NeighborGroups(forums, {HAS_MEMBER}, Outgoing)`, project
  `[(Outgoing, IS_LOCATED_IN), (Outgoing, IS_PART_OF)]`, `top_by_size(100, tie=flid)`
  -> 100 `[forum_id, largest_cohort_size]` in ranked order.
- `fold_via_top100`: `FoldVia({REPLY_OF}, Outgoing, NeighborVia(HAS_CREATOR, Outgoing))`,
  top 100 `[a,b,count]` by count desc then (a,b) asc.
- `common_neighbor_counts`: sources = 50 smallest Person ids, `Both`, `KNOWS`,
  targets = all Persons, self-pairs dropped -> `[s,t,count]` sorted by (s,t).
- `aggregate.by_birth_month`: `Aggregate("Person").By("bmon").Sum("pday")` ->
  `[bmon, count, sum_pday]` sorted by bmon.
- `aggregate.by_creation_year`: `Aggregate("Post","Comment").TemporalComponent("ms", Year)`
  -> `[year, count, sum]` sorted by year (Person has no epoch-millis col, so the
  Year rollup runs on Message `ms`).
- `weighted_shortest_path`: 10 pairs `(persons[i], persons[i+25])` i in 0..10,
  `Both`, `KNOWS`, weight 1.0. **Costs are dumped as `cost_bits` = f64 `to_bits()`
  u64** (`null` = unreachable); decode with `math.Float64frombits`, don't parse text.

Determinism contract (the fixture relies on it): every list is sorted, ties by id
ascending; node/rel ids are dense internal ids, identical on both engines after
loading the same `sf1.rcpg`. A fixture section that's absent is skipped (keep that
day-one-friendly pattern already in `loadLDBCExpected`).

## Deliverable 2 -- emit timings in the suite schema

A bench entry point (extend `bench_test.go`, or a `scripts/` cmd -- your call)
that, for each of the 6 kernels: (a) verifies output == fixture (the gate), then
(b) times it (warm; median of a few runs) and appends one record per kernel to an
append-only JSONL file. Record shape (from the suite's `python/cypher/timings.py`):

```json
{"family":"BI","query":"neighbor_groups","variant":"committed",
 "engine":"gochickpeas (go)","warmth":"warm","ms":<median>,"rows":<n>,"sf":1,
 "shape":"native kernel","parity":"MATCH",
 "engineCommit":"<GO repo HEAD 7-hex>","engineDate":"<go commit ISO date>",
 "engineDateTime":"<go commit ISO datetime>","engineSubject":"<go commit subject>",
 "ldbcCommit":null,"ldbcDate":null,"ldbcDirty":false,
 "measuredDate":"<UTC date>","source":"emitted",
 "msMin":..,"msP25":..,"msP75":..,"msN":..,
 "meta":{"port":"gochickpeas","coreConformance":"v0.1.0",
         "coreCommit":"<fixture meta.core_commit>","goVersion":"<runtime.Version()>",
         "nodes":<n>,"rels":<n>}}
```

Key stamping decisions (these are what make it a clean series on the chart):

- **`engineCommit` = the GO repo's HEAD**, not core. gochickpeas tracks its own
  progress; its x-axis is its own commit timeline (exactly how ragedb sits as its
  own series). Put the core-conformance level + the fixture's `core_commit` in
  `meta` so the point is self-describing.
- **`parity` gates the emit**: only write a record when the kernel output MATCHed
  the fixture. A DIFF must fail the run, not publish a green timing.
- **`engine` = `"gochickpeas (go)"`** verbatim -- that string is the column.
- Pick `family`/`query` labels that read well next to the rcp-native rows. The 6
  kernels map to BI/IC-ish shapes (neighbor_groups~BI-Q4, fold_via~IC14/Q19); a
  `family:"native"` bucket is also fine -- coordinate the exact labels with the
  ldbc side if you want them to line up in a specific cell.
- `msMin/msP25/msP75/msN` are optional but match the suite's timing-rigor
  convention (median-of-n with a visible spread band); include if cheap.

Write to a dedicated file (e.g. `emitted_gochickpeas.jsonl` in the repo root or a
`bench-out/` dir). The ldbc side copies it into `viz/data/emitted_gochickpeas.jsonl`
via `viz/import_gochickpeas.sh` -- so just tell them the path (ping back on this
task or note it in the file's header comment).

**Pickup contract is LIVE (their 257, 2026-07-03):** `viz/import_gochickpeas.sh` runs on every
site deploy and looks for `emitted_gochickpeas.jsonl` at THIS repo's root, then under
`bench-out/` -- write to either and your records flow to the site on the next deploy (overwrite
semantics: keep your full history in the file; their extract dedups last-write-wins per
cell-commit). A third location needs a one-line ping back. The two engine columns
(`gochickpeas (go)` teal next to the native floors, `gochickpeas (gql)` next to the cypher
engines) + the methodology note are already deployed and waiting.

## Deliverable 3 -- GQL runner (phased; gated on the gql engine)

Once the GQL engine can execute (011 is scaffold-stage today), add a
manifest-driven runner. **The first manifest is SHIPPED** (their task 258
phase 1, 2026-07-02): BI 12/12 canonical queries at
`/Users/efreeman/rustychickpeas-ldbc/viz/data/gql_variants.tsv`
(`family, query, variant, graph, refhash, norm, gql` per row; hashes validated
end-to-end against the live rcp-cypher engine). The graph column points at
`.../export/sf1_canonical.rcpg` -- the canonical-PROPERTY-NAME snapshot (NOT
`sf1.rcpg`, whose property keys are mirror-style and serve the native kernels
only). Dialect (`gqlv0`) + parity contract: their `gql/README.md`; per-query
notes in `gql/bi/q*.gql`; the texts use `RETURN..NEXT` composition, `LET`,
`FILTER`, `FOR`, quantified edges `{0,}`, `IS LABELED`, plus documented
cypher-inherited extensions -- if any construct fights your parser design,
ping back and they revise the texts (hashes don't move). Known caveat: Q16's
ref is legitimately empty on SF1 (weak gate). The runner, per manifest row:

- executes the GQL text against the row's `graph` (.rcpg path in the manifest),
- normalizes + hashes the result rows per **rowhash/v1**
  (`python/cypher/rowhash.py` in their repo is the normative spec: cell
  encoding, sort, sha256[:16]; port it once and reproduce its self-test
  vectors -- `python -m cypher.rowhash`, vector hash `8356f03559b181d9`),
  applying the row's `norm` ops first (e.g. `col2:msday` = int-divide col 2 by
  86400000), and compares to `refhash` -- **the parity gate**,
- on MATCH, times it (warm median, same spread fields) and emits with
  `engine: "gochickpeas (gql)"`, same stamping as Deliverable 2; on DIFF or a
  `blocked:` manifest row, no timing -- fail/skip loudly.

Query ids in the manifest match the suite's cells (BI Q1.., IC1.., FinBench
TR1..) so `gochickpeas (gql)` lands next to `rcp-cypher (rust)` in the same
(family, query) cell. Coverage is incremental by design -- the manifest supports
partial sets, and the ldbc side ships easiest-family-first so the executor has
real targets to grow against. Translation flexibility is theirs; if a query
needs a GQL feature the engine lacks, ping back and they mark the row blocked.

## Notes

- Native kernels compare against `rcp-native (rust)`; GQL compares against
  `rcp-cypher (rust)`. Same rcpg, same machine -- the pair isolates go-vs-rust
  runtime on the floor, and engine-vs-engine on the query layer.
- Regenerate the fixture (both files) from the Rust side with
  `cargo run --release --bin export_gochickpeas` if core moves; the JSON stamps
  the core commit it was built at. If the RCPG codec version ever diverges from
  the fixture's, flag it and the ldbc side re-exports at a pinned commit.
- Leave this task file as-is if you're mid-`011_gql_scaffold`; it has no ordering
  dependency on the gql work.

## Update 2026-07-03: IC family shipped (manifest now BI 12 + IC 20)

- `viz/data/gql_variants.tsv` grew the IC rows: IC1..IC13 + IS1..IS7 (20 runnable; IC14 is a
  `blocked:` file with no manifest row -- weighted shortest path). Same contract as BI; all IC
  `norm` cells are `-` (no norm ops).
- New gqlv0 constructs the IC texts use (mapping table updated in their `gql/README.md`):
  `ANY SHORTEST` path selector over quantified edges (`MATCH p = ANY SHORTEST (a)-[:KNOWS]-{1,3}(f)`,
  endpoints pre-bound; IC1/IC13), `length(p)` = path rel count, `COUNT { <pattern> }` subquery
  (IC10), aggregate-DISTINCT (`collect_list(DISTINCT ..)`, IC4), node-valued list membership
  (`t IN before`, IC4).
- **The shared snapshot was REBUILT** (`export/sf1_canonical.rcpg`, now 635 MB): it additionally
  carries the real nullable message `content` text (IS4), `LIKES.creationDate` /
  `HAS_MEMBER.joinDate` (epoch-ms) and `WORK_AT.workFrom` / `STUDY_AT.classYear` (year) rel
  props, and Person `bmon`/`bdom` birthday month/day columns (IC10). Re-copy it if you cached a
  pre-2026-07-03 version; BI refhashes did not move (verbatim re-validated 12/12 on the rebuild).
- Every IC refhash is validated end-to-end: the canonical-schema cypher twins
  (`python/cypher/ic_canonical.py` in their repo) reproduce all 20 hashes live on this exact
  snapshot (`python -m cypher.ic_canonical` -> 20/20 MATCH).
- **Path modes (gqlv0 update, same day):** bare GQL quantifiers mean **WALK** (the ISO default;
  rels/nodes may repeat) -- the rcp engine flipped to this at their 847b021, and the manifest
  texts now state **`TRAIL`** explicitly where per-path cypher semantics are intended
  (`MATCH TRAIL (p ..)-[:KNOWS]-{1,2}(f ..)` in IC3/IC5/IC6/IC9/IC11). The runner must honor the
  TRAIL prefix (no repeated rel per path); bare spellings left in the texts are mode-invariant
  by construction (unbounded reachable-set, EXISTS-over-DAG, or under ANY SHORTEST) and noted
  per file. On SF1 these five are row-identical either way (verified), so a walk-only executor
  would still hash-MATCH today -- but implement TRAIL, not that coincidence.

## Update 2026-07-03b: FinBench shipped -- the manifest is COMPLETE (49 rows, all 3 families)

- `gql_variants.tsv` is now BI 12 + IC 20 + FinBench 17 (CR1-7, CR9-12, SR1-6; CR8 is the
  FinBench `blocked:` row -- stateful claim-order BFS, gated in the rcp mirror too). That is the
  full 258 scope: SPB (SPARQL) and GA (kernels) have no cypher to translate.
- **New graph artifact:** FinBench rows point at `.../export/finbench_sf10_canonical.rcpg`
  (337 MB; official datagen property names -- rel `createTime`/`amount` (`loanAmount` on apply),
  node `isBlocked`/`loanAmount`/`createTime`/`type` -- over the official lowercase rel types).
  FinBench node ids are unique only per label (datagen); resolve seeds label-scoped.
- **Two NEW rowhash/v1 norm ops your port must implement** (spec + self-test vectors updated in
  their `python/cypher/rowhash.py`): `round3` (round float cells, recursing into lists, to 3
  decimals -- the FinBench refs are 3-decimal; half-way ties not exercised by the corpus) and
  `unwrap1` (single-list-cell rows -> the list itself; CR5's variable-arity node-trace). Every
  FinBench row carries `round3` except CR5 (`unwrap1`).
- **Path modes in the FinBench texts:** named-path form `MATCH p = TRAIL (s)<-[:transfer]-{1,3}(o)`
  (CR1/CR2 -- per-path monotonic-createTime filters over `rels(p)`), `p = ACYCLIC ...` (CR5,
  replaces a hand-written all-nodes-distinct filter), `ANY SHORTEST ->{1,}` (CR3). Plus list
  machinery spelled as in cypher: comprehensions, `all()`/`single()`, `range()`, `size()`,
  indexing.
- Weak gates to know about: CR12's ref is legitimately EMPTY at SF10 (empty-set hash), like BI
  Q16. All 17 refhashes are twin-validated live on this exact snapshot
  (`python -m cypher.finbench_canonical` -> 17/17 MATCH).
- Full-window note: the committed seeds pin ws/we = i64::MIN/MAX, so the texts ELIDE the
  ts-window predicates (also dodges the `-9223372036854775808` literal parse trap). If a
  sub-window seed ever ships, texts + refs regenerate together.
