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
