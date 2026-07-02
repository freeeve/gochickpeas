# 009: Full-text + geo (M8)

BM25 field index + tokenizer (port fulltext.rs), k-d tree + haversine +
radius/bbox/knn (port geo.rs), lazy index plumbing on Snapshot. Hand-rolled,
no new deps.
