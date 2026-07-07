# 047 — go-gql: multi-pattern-join query planning perf (BI Q17, and Q8 is slow)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** Correctness is fine;
these run but are far slower than the same shapes on rcp/native, pointing at join-order / cardinality
planning.

## BI Q17 — effectively unrunnable (>2 min, killed)
Six comma-joined patterns + two `-[:REPLY_OF]->{0,}` var-length legs + a `duration` predicate and a
`NOT EXISTS`:
```
MATCH (tag {name:'Slavoj_Žižek'}),
      (person1)<-[:HAS_CREATOR]-(message1)-[:REPLY_OF]->{0,}(post1)<-[:CONTAINER_OF]-(forum1),
      (message1)-[:HAS_TAG]->(tag),
      (forum1)-[:HAS_MEMBER]->(person2)<-[:HAS_CREATOR]-(comment)-[:HAS_TAG]->(tag),
      (forum1)-[:HAS_MEMBER]->(person3)<-[:HAS_CREATOR]-(message2),
      (comment)-[:REPLY_OF]->(message2)-[:REPLY_OF]->{0,}(post2)<-[:CONTAINER_OF]-(forum2)
… WHERE forum1 <> forum2 AND message2.creationDate > message1.creationDate + duration({hours:4})
        AND NOT EXISTS { MATCH (forum2)-[:HAS_MEMBER]->(person1) }
```
gqlbench killed it after >2 min at SF1 (other BI queries are ms–seconds). `gql/bi/q17.gql` is authored +
correct but `-- blocked:` on this. Likely no cost-based ordering of the comma-joined patterns (it seems to
expand a huge intermediate before the selective `HAS_TAG`/`tag` anchors prune it).

## BI Q8 — slow but passes (11.7 s)
`MATCH (person:Person) FILTER EXISTS{…} OR EXISTS{…}` does a full Person scan with two EXISTS probes each.
Correct (MATCH) but a seek/anchor on the tag side would be far cheaper.

## Ask
Cost-based join ordering for comma-joined multi-pattern MATCH (anchor on the most selective bound node --
here `tag {name:…}` -- and grow outward), and an anchored plan for `MATCH (x) FILTER EXISTS{(bound)-…-(x)}`
instead of a full label scan. Repro: `cmd/gqlbench -only Q17` / `-only Q8` against `sf1_canonical.rcpg`.

## Disposition (gochickpeas session, 2026-07-06): both fixed -- plus TWO hidden correctness gaps

**Q8**: the EXISTS probes were quadratic because a both-ends-anchored subquery pattern always walked from
its written start (the hot tag). Probes now pick the cheaper side per row by actual endpoint degree (new
O(1) `Snapshot.Degree`). Q8's person-filter segment at SF1: **16.5s -> 0.8s**.

**Q17**: three stacked problems, only one of them perf.
1. *Perf*: join reordering only costed pattern ENDPOINTS, so a pattern whose selective node was a bound
   interior (`message1`) anchored at `forum1` and expanded a 182M-row intermediate; and an anchor-cost
   tie between two 0-cost patterns broke by written order, placing a 25B-fanout pattern before a
   1-fanout one. Fixed generally: interior-aware anchor costs, fan-out tie-breaks, and a bound-aware
   interior-split pass after reordering (var-length allowed). Q17 now completes.
2. *Correctness A*: `creationDate + duration({hours: 4})` evaluated to Null (creationDate is epoch-millis
   Int; Int +/- Duration was an unported Rust fix, their tasks/151) -- the WHERE dropped every row, so
   Q17 would have returned 0 rows at any speed.
3. *Correctness B*: counts came out +2 vs the ref because MATCH-scope relationship uniqueness (ISO GQL's
   DIFFERENT EDGES default; the Rust engine's tasks/219) was never ported -- person2 == person3 reusing
   one HAS_MEMBER edge across two comma patterns was wrongly admitted. Ported fully (scope marking gated
   on rel-type intersection; used-pair env threaded across chained stages; var-length check/contribute).

Result: **Q17 returns the reference's exact 10 rows in ~24s** at SF1 (was: killed >2min, and wrong twice
over had it finished). Parity gate 78/78 MATCH, full suite + 45s fuzz green. Q17 and Q8 can come off
`-- blocked:` and into the manifest.
