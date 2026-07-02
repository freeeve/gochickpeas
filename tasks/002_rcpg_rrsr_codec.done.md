# 002: rcpg + rrsr codec (M1)

Byte-compatible RCPG codec (Parse/Write, topology-only options, lazy
Directory/SectionFetch/ParseLazy/PlanSections) and RRSR record store.
Conformance: golden corpus from rustychickpeas-format parses to manifest
facts, rewrites byte-identically, and Go-built graphs match golden bytes;
reverse direction verified by Rust's go_interop test. Fuzz targets green.
Deferred to a later task: CsrLayout / CSR-skeleton / working-set machinery
(the range-faulted adjacency path); SectionFetch + directory planning lock
the interface it builds on.
