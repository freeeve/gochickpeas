# 023 -- gql Prepared + PlanCache (M21)

Port the public Prepared API and the two-layer byte-budgeted LRU plan
cache (rustychickpeas-gql src/cache.rs + lib.rs): L1 keyed on verbatim
query text, L2 on the autoparam template fingerprint, both sharing one
template plan (value-independent by the M13 cost-probe abstention audit);
LRU eviction with 90%-of-budget hysteresis; Prepared = parse + desugar +
autoparam + plan once, re-executable with named params.

Gate: ported Rust template-sharing and byte-bound-eviction tests,
Prepared round trips, cached-vs-uncached row equality, concurrent cache
use under -race; >=80% coverage.
