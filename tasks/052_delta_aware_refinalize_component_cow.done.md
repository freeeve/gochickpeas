# 052 -- delta-aware refinalize: component-level copy-on-write across snapshots

## Motivation

Update mode (thaw -> edit -> Finalize -> Manager swap) rebuilds everything on
both ends: thaw inverts every label bitmap, Kahn-restages all m rels to
reproduce both CSR orders, and rematerializes every column into staged pairs;
Finalize rebuilds all of it from staging. A one-property edit costs O(n+m).

Snapshots are strictly immutable and the Manager swap is the commit point --
exactly the invariant that makes structural sharing safe. Aliasing arrays from
the source snapshot into the successor can never be observed as mutation, and
Go's GC handles shared-backing lifetimes. This is a general engine improvement
(fires on update shape, never on query identity), so it clears the
no-overfitting bar.

## Scope

Component-granularity sharing only ("tier 1"). No chunked/persistent CSR, no
finger trees, no delta overlay on the read path -- traversal hot loops stay
flat-CSR scans, untouched. A chunked/COW CSR for rel-heavy churn is a separate
future task, to be considered only if measurements show rel churn dominating.

### 1. Dirty tracking in the Builder

A thawed Builder records which components diverge from its source snapshot:

- per-key dirty sets for node columns and rel columns (any Set/Remove on that
  key marks it),
- per-label / per-type dirty marks for labelIndex / typeIndex,
- a single `relsDirty` bool (any rel add/remove),
- node-set changes that affect label bitmaps or column coverage mark the
  affected components.

Builders not created by thaw have no source snapshot and finalize exactly as
today.

### 2. Aliasing in Finalize

When a source snapshot is present, Finalize aliases clean components instead
of rebuilding:

- `relsDirty == false` -> alias both CSRs (outOffsets/outNbrs/outTypes,
  inOffsets/inNbrs/inTypes) and `inToOut` wholesale. Property-only updates
  skip the entire O(m) restage/rebuild.
- untouched `columns[key]` / `relColumns[key]` -> alias the Column value.
  Bonus: a shared untouched column is carried bit-identical, sidestepping the
  dense-column "never set vs zero" lossy corner (thaw.go) for clean columns.
- untouched labelIndex / typeIndex entries -> alias the *nodeset.Set.
- atoms: append-only by construction (interner is seeded from the source
  table), so the atom table can share its prefix or alias outright when no
  new atoms interned.

### 3. Lazy-cache carry-forward (possibly the biggest real-world win)

propIndex, fulltextIndex, geoIndex, rootsViaIndex are expensive lazy builds
that currently die with the old snapshot. For (label, key) / (dir, type)
entries whose underlying components are clean, hand the built cache entries to
the successor at Finalize. Entries touching dirty components are dropped.

### 4. Lazy thaw (stretch, evaluate after 1-3)

With dirty tracking in place, thaw itself no longer needs to eagerly
rematerialize clean components -- e.g. defer the O(m) Kahn rel restage until
the first rel mutation. Only worth it if profiles show thaw cost still
matters once refinalize aliases; keep out of the first cut if it complicates
builder invariants.

## Non-goals

- Any persistent tree structure (finger tree, RRB vector, HAMT) -- the maps
  are small and the big arrays are scanned, not split/concatenated.
- Delta overlay / merge-on-read -- a read-path tax on every query to speed up
  writes is the wrong trade for this engine.
- Serialization changes: WriteRCPG still walks flat arrays; RCPG byte-compat
  with the Rust sibling is untouched.

## Verification

- No-edit thaw -> Finalize -> WriteRCPG stays byte-identical (existing
  round-trip property), and additionally asserts aliasing occurred (shared
  backing arrays) rather than an equal rebuild.
- Property-only edit: CSRs alias (pointer equality on backing arrays), edited
  column rebuilt, all other columns alias.
- Rel edit: full CSR rebuild, clean columns still alias.
- Mutation-after-alias safety: fuzz interleaved edits + finalize cycles
  confirming a successor snapshot never mutates its source (removal fuzz
  harness extends naturally).
- Parity gate green (gql MATCH + native manifests) via /verify.
- Bench: micro-bench thaw+refinalize for (a) no-edit, (b) 1-property edit,
  (c) 1-rel edit on the LDBC snapshot; record before/after.

## Outcome

Scope 1-3 landed in 90f4489; scope 4 (lazy thaw) deferred, see below.

Dirty tracking and the alias plan live in a new `cow.go`; `Builder` gained
`src`, `srcRelToOutCSR`, three dirty sets, and `relsDirty`. `Finalize` grew an
`aliasPlan` that decides per component, and `finalizeLabels` /
`finalizeNodeColumns` / `finalizeRelColumns` / `buildTypeIndex` split out of
it. Thaw records the source last, so the restage itself dirties nothing.

What aliases:

- both CSRs + `inToOut` + typeIndex when the rel set AND the id space are
  clean (an id-space change rewrites both offset arrays, so `relsDirty` alone
  is not sufficient -- that was the subtlest condition here)
- node/rel columns per untouched key; rel columns only alongside the CSR,
  since they are keyed by outgoing-CSR position. A rebuilt rel column on a
  clean CSR remaps through `srcRelToOutCSR` (the inverted thaw map) rather
  than a position map from a CSR build that never ran.
- label bitmaps per untouched label; no dirty label at all skips the O(n)
  label scan outright
- the atom table when nothing new was interned (an LDBC-scale copy)
- propIndex / fulltext / geo / rootsVia / colPos caches keyed on aliased
  components

Deliberate conservatism: `RemoveNode` marks the rel set dirty unconditionally,
because the Finalize cascade rescans every staged rel for incidence anyway.

`relStats` is NOT carried forward even on a clean CSR: it is a `sync.OnceValue`
closure over the source snapshot, so sharing it would chain each successor to
its whole ancestor snapshot and defeat the GC. Carrying it needs the built
value, not the thunk -- a small follow-up if it ever shows up in a profile.

### Bench (50k nodes / 200k rels, `BenchmarkRefinalize`, aliased vs rebuilt)

The benchmark keeps both paths so the before/after stays reproducible in-repo;
thaw is excluded from the timer (it remains O(n+m), see below).

| edit         | rebuilt | aliased | allocs rebuilt -> aliased |
|--------------|---------|---------|---------------------------|
| no edit      | 7.06ms  | 21.9us  | 17.1MB -> 2.3KB           |
| one property | 7.48ms  | 238us   | 17.1MB -> 410KB           |
| one rel      | 9.75ms  | 8.38ms  | 17.9MB -> 16.3MB          |

The one-rel row is the expected floor: both CSRs and every rel column rebuild;
node columns, labels, and atoms still alias.

### Verification

- `FuzzRefinalizeMatchesRebuild` is the gate the CLAUDE.md parity rule asks
  for: the same builder + edit script finalized twice, once aliasing and once
  with `src` cleared (forcing the general path), must produce byte-identical
  RCPG, and the source's bytes must be unchanged after both. 2.3M execs clean.
- Component-level alias assertions (`cow_test.go`) on backing-array identity
  for the no-edit, property-edit, rel-edit, id-space-growth, prop-removal,
  atom-interning, and cache-carry cases.
- Parity gate: 89/89 MATCH, 0 DIFF, 0 SKIP.
- Public-API drive: thaw -> edit -> Finalize -> `gql.Run`, chained twice; the
  source keeps reading its old values and the eager index carries forward.

### Behavior change worth knowing

An unedited thaw of a snapshot READ FROM A FILE now reproduces that file's
bytes exactly rather than finalize's canonical re-encoding of them, because
clean components carry through untouched. The `big` corpus fixture carries an
`optimize()`d roaring run container in its label index; it used to need
normalizing before comparison and now round-trips byte-identically, so it
moved from `TestThawRoundTripFinalizeNormalized` into
`TestThawRoundTripByteIdentical`. Editing the label rebuilds the bitmap with
finalize's plain construction, as before.

### Follow-ups

- Scope 4 (lazy thaw) not attempted: with refinalize aliasing, thaw is now the
  whole cost of a no-edit cycle (the O(m) Kahn restage plus rematerializing
  every column into staged pairs). That is where the next win is, and it is
  worth its own task now that dirty tracking exists to lean on.
- `RemoveNode` on an isolated node forces a CSR rebuild; a per-node incident
  rel index would let it stay clean.
