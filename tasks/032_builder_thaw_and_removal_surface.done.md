# 032: Builder thaw (Snapshot -> Builder) + removal surface

Enable writes as **read-modify-refinalize-swap**: thaw an existing Snapshot
(or RCPG file) back into a Builder, edit it (including removals), Finalize a
new Snapshot, register it in the Manager. Readers keep whatever snapshot they
hold; the Manager swap is the commit. Snapshots stay immutable -- every
lock-free assumption (lazy caches, shared nodesets, `graph.Native` kernel
paths in gql) survives untouched.

Explicitly OUT of scope: in-place mutation or a delta/overlay snapshot type.
An overlay would poison every hot path with merge logic or fall off the
`graph.Native` fast paths. If refinalize latency ever matters, the follow-up
is incremental Finalize (copy-on-write reuse of untouched columns/CSR runs),
not an overlay.

Counterpart task filed in rustychickpeas as `tasks/223_core_builder_thaw_and_removals.md`
(untracked, per the cross-repo protocol) so the Rust side can mirror.

## Phase 1: thaw

New constructor in builder.go (or a new thaw.go):

- `NewBuilderFromSnapshot(g *Snapshot) *Builder`

Purely additive; no existing code changes. Reconstruction per field:

- **Interner from Atoms** -- copy the id-ordered string table into a fresh
  Interner, preserving atom ids exactly. This is the keystone: labels, rel
  types, property keys, and string values stay valid because ids never move.
- **Rels** -- reconstruct a staging order consistent with BOTH CSR
  directions' per-node orders, so a no-edit `thaw -> Finalize -> WriteRCPG`
  round trip is byte-identical (naive out-CSR order reproduces the outgoing
  CSR but can permute rels within an incoming node's range). Such a linear
  extension exists by construction (the original insertion order is one);
  find it with the same k-th-occurrence pairing as `computeInToOutFromCSR`
  (serialize.go). Rebuild `degOut`/`degIn`/`relTypes`/`knownNodes` alongside.
- **Labels** -- invert the per-label bitmaps into per-node lists. Per-node
  label order was never serialized and does not affect Finalize output; atom
  order is fine.
- **Node columns** -- enumerate each column variant into staged pairs
  (`Entries()` for rank layouts; dense/sparse direct).
- **Rel columns** -- stored by outgoing-CSR position; remap to builder rel
  index via the reconstruction-order permutation.
- **Misc** -- `version` carries over; `nextNodeID` = max known id + 1.

Known lossiness (inherited from the format, document on the constructor):

- Dense i64/f64/bool columns cannot distinguish "never set" from the zero
  value, so thaw stages a pair for every position (dense str is exempt:
  atom 0 = missing). Refinalize keeps them dense unless edits drop them
  below the 80% threshold, at which point storage selection does the right
  thing automatically.
- Ghost nodes: an isolated, unlabeled, propertyless node contributes to
  `nNodes` but leaves no identifiable trace. Rebuild `knownNodes` from
  labels ∪ rel endpoints ∪ column positions; if its cardinality falls short
  of `g.NodeCount()`, the ghosts' identities are unrecoverable -- accept and
  document (rcpg itself already reads back this way).

## Phase 2: removal surface

New Builder methods:

- `RemoveProp(node NodeID, key string) bool`
- `RemoveRelProp(u, v NodeID, relType, key string) error` +
  `RemoveRelPropAt(relIdx int, key string) error`
- `RemoveRel(relIdx int) error`
- `RemoveNode(id NodeID) bool` (detach-delete)

Mechanics:

- **Prop removal** sweeps ALL staged occurrences across ALL FOUR typed
  columns. The existing `removePair` (used by UpdateProp) removes only the
  first pair in one typed column -- both gaps would resurrect stale values
  at Finalize.
- **Pre-existing quirk to fix in the same pass:** a key staged under two
  value types lands in two typed column maps, and Finalize's four loops
  each assign `g.columns[key]`, so the winner is loop order (str > bool >
  f64 > i64), not write order. `UpdateProp` only clears the NEW type's
  column, so it does not prevent this. Decide last-write-wins across types
  and enforce it (UpdateProp and the new removals sweep all four).
- **RemoveRel: tombstone, never compact.** Swap-removing from `b.rels`
  would invalidate every rel index AddRel handed out, the `relIndex` map,
  and staged rel-prop ids. Keep a `removedRels` roaring bitmap; decrement
  `degOut`/`degIn` immediately (endpoints known); nil out `relIndex` so it
  lazily rebuilds skipping tombstones.
- **RemoveNode: lazy cascade.** No per-node rel index exists, so eager
  cascade is O(m) per call. Keep a `removedNodes` bitmap; clear labels and
  `knownNodes` membership eagerly; incident rels die at Finalize.
- **Finalize compaction pass** (only when tombstones exist): one O(m +
  pairs) filter -- skip rels that are tombstoned or touch a removed node,
  recompute degrees for cascaded rels, drop staged rel props on dead rels
  in the rel-index -> CSR remap, filter removed nodes' pairs out of every
  node column.

Semantics decisions (document on the methods):

- **Ids retire, never reuse.** `nextNodeID` does not rewind. Finalize
  already sizes the CSR span from `knownNodes.Maximum()+1`, so removing the
  max node shrinks the id space naturally; sparse ids are first-class.
- **Removal is not a permanent tombstone.** A later `AddRel(u, removed, t)`
  auto-registers the endpoint (existing builder behavior) and resurrects
  the node unlabeled and propertyless -- the `removedNodes` bit MUST clear
  then, or the Finalize cascade kills the new rel.

## Done when

- No-edit round trip `ReadRCPG -> NewBuilderFromSnapshot -> Finalize ->
  WriteRCPG` is byte-identical across the conformance corpus shapes: sparse
  ids, parallel rels (same endpoints + type), rel props, dense/sparse/rank
  columns of all four kinds, float specials, unaligned dense bool, version
  string.
- Removal semantics covered table-driven: prop remove across types and
  duplicate staged writes; rel tombstones with parallel rels and rel props;
  detach-delete cascades; resurrection via AddRel; dense column crossing
  the 80% threshold after removals; removal x thaw interactions.
- Fuzz: random add/remove/set sequences against a naive map-based reference
  model (oracle), asserting post-Finalize snapshot equality.
- Godoc example of the Manager write loop (read -> thaw -> edit -> Finalize
  -> AddSnapshotWithVersion).
- PARITY.md notes the deliberate divergence (Go ships ahead of Rust; Rust
  counterpart tracked in their tasks/223).
- gofmt -s / vet / race clean, >80% coverage on the new code.

## Outcome (shipped)

thaw.go (NewBuilderFromSnapshot + Kahn linear-extension rel restaging),
removal.go (removal surface + Finalize compaction), UpdateProp cross-type
sweep fix, tests (thaw round trips, removal tables, fuzz oracle with
mid-run thaw, Manager write-loop example), PARITY.md go-ahead section.
Findings appended to rustychickpeas tasks/223. Deviations from the spec
above, discovered against the conformance corpus and fuzz oracle:

- Dense STR columns are NOT exempt: thaw stages every position including
  atom 0 (all_columns' 7-of-13 dense str would otherwise refinalize
  sparse; the golden is itself built with explicit atom-0 writes, and the
  read layer reports those positions present-"").
- removedNodes is a node -> watermark (staged rel count at removal) map,
  not a plain bitmap: clearing a bit on resurrection would also resurrect
  pre-removal incident rels. The watermark keeps old rels dead while rels
  added after resurrection survive; node props purge eagerly at RemoveNode
  so a resurrected node is truly propertyless (and Finalize then needs no
  node-column filter).
- Byte-identity vs raw golden holds for sparse_ids / all_columns /
  multi_label_types / topology_only; small (hand-built 1-of-2 sparse rel
  column), empty (offsets [0] vs finalize's [0,0]) and big (optimize()d
  run container no finalize emits) compare against finalize-normalized
  bytes instead, plus idempotence.
- Cross-node mixed-type keys remain loop-order-resolved (one column per
  key in the format); per-node last-write-wins is what UpdateProp/removals
  enforce. Documented on SetProp.
