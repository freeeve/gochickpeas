# 004: Snapshot read path (M3)

Snapshot struct + GraphSection bridge (FromGraphSection/ToGraphSection,
inToOut reconstruction by k-th pairing -- see core's
compute_in_to_out_from_csr), RelMatch + Neighbors/Rels iter.Seq iterators,
Col/Prop columnar access (12 storage variants, exact dense/sparse/rank
semantics), lazy (label,key)->value->Set property indexes, schema
introspection, RelStats via sync.OnceValue. Size all scratch by
csr_id_space, never NNodes (sparse-id hazard).
