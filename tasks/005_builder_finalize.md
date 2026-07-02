# 005: Builder + Finalize (M4)

AddNode/AddRel, typed + generic prop setters, node/rel deduplication,
parallel CSR build, dense/sparse/rank column selection -- port the
thresholds from graph_builder/finalize.rs exactly (fill-ratio bands,
RANK_SELECT_MIN_LEN = 1_000_000) so Go-built snapshots serialize
byte-identically to Rust-built ones for the same input.
