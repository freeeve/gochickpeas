# 003: Core types (M2)

NodeID/Direction/Label/RelType, comparable Value (ValueId port, F64 by
bits), Atoms table + build-side Interner (hand-rolled), internal/bitset,
nodeset.Set (roaring-only behind private repr) + package-level ParFold,
internal/parallel (For/Fold/Join, chunks = workers x 4, per-chunk
accumulators). Port the NodeSet xorshift differential suite from
rustychickpeas-core/src/bitmap.rs.
