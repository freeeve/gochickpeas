# 063 -- aggregate_run through+sum must be per-source (sum once per neighbor group, deduped across parallel same-type rels) while count stays per-rel (mirror rustychickpeas 240)

Filed from rustychickpeas on 2026-07-10 (cross-repo ask). Spec and Eve's
split decision live in rustychickpeas tasks/240 (Rust fix 0bea7ee).

## Outcome (2026-07-10)

Mirrored the Rust split in aggregate_run's through branch: count stays
per RELATIONSHIP (each rel bumps its neighbor group's count), while a
source's sum value contributes once per DISTINCT matched neighbor --
collected into a per-accumulator scratch, sorted + compacted per node,
then added to each distinct neighbor's group -- so parallel same-type
rels (the Both-direction / undirected-stored-both-ways case) no longer
inflate sums by the source's degree. The loop iterates RelsMatch rather
than NeighborsMatch so per-rel count multiplicity is independent of
062's coming set-semantics flip on the neighbor-id surfaces. The
non-through path is untouched.

TestAggregateThroughSumPerSourceCountPerRel mirrors Rust's fan-out
fixture exactly: Post0 (w=10) -> Tag2 via two parallel hasTag rels +
Tag3, Post1 (w=100) -> Tag2; count(Tag2)=3, sum(Tag2)=110 (not the
inflated 120), count/sum(Tag3)=1/10. Count-only Through behavior is
pinned unchanged by the existing TestAggregateThrough. Full suite
green; both parity gates 89/89 MATCH (no LDBC kernel chains
through+sum, matching the Rust side's caller survey).
