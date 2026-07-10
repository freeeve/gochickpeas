# 062 -- Snapshot neighbor id surfaces must return a deduped ascending set (mirror rustychickpeas 242 item 2)

Filed from rustychickpeas on 2026-07-10 (cross-repo ask). Eve's decision
(rustychickpeas tasks/242): SET SEMANTICS everywhere -- deduped,
ASCENDING -- for neighbor-ID surfaces; multiplicity stays available via
the relationship surfaces. Rust fix 5c859b0 (Python bindings).

## Go-twin scoping (2026-07-10) -- why this is not a drop-in mirror

Rust changed COLD Python-binding surfaces. Go's equivalents
(Snapshot.Neighbors / NeighborsMatch / AppendNeighborsMatch) are ALSO
the hot executor seam, where two properties are load-bearing:

- **Multiplicity** is GQL row semantics: pattern expansion, COUNT
  subqueries, and through-count must see one hit per relationship.
- **Iteration order** breaks path tie-breaking: spWalk's parent[v]=u
  keeps the FIRST reach, and the rowhash gate pins exact path rows --
  ascending order would change which equal-hop parent wins.

Plan (phased; 063 already made its aggregate loop flip-proof):

1. Audit + migrate every multiplicity- or order-sensitive INTERNAL
   caller to the relationship surfaces (RelsMatch /
   AppendRelationshipsMatched) or to an order-preserving unexported
   append. ~145 call sites: gql seam (6), core kernels.go (12),
   analytics/search/neighborgroups (13), aggregate_run (done),
   internal/ldbc kernels (~110 -- LDBC data has no parallel same-type
   rels except stored-both-ways KNOWS under Both; per-site check for
   order/dup sensitivity, gate-verified).
2. Flip the public neighbor-ID surfaces to dedup + ascending (scratch
   sort+compact on the way out; typed views stay order-preserving
   internally).
3. Both parity gates 89/89 + a set-contract unit test (parallel-rel
   fixture: Neighbors returns each neighbor once, ascending; RelsMatch
   still yields per rel).

Phase 1's audit is the bulk and wants an uninterrupted pass; scheduled
for the next firing.
