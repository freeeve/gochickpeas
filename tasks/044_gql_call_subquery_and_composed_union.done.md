# 044 — go-gql: `CALL { }` subquery + composed-UNION-then-continue (blocks BI Q4)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** Building the pure-GQL
baseline; **BI Q4** is the one query that can't be expressed with what go-gql exposes today.

## The shape Q4 needs
Q4 builds a top-100 forum list, then over two row-sources computes per-person message counts and **sums
them**:
```
… WITH collect(topForum) AS topForums
CALL {
  WITH topForums UNWIND topForums AS f
  MATCH (f)-[:CONTAINER_OF]->(post)<-[:REPLY_OF*0..]-(m)-[:HAS_CREATOR]->(person)<-[:HAS_MEMBER]-(f2)
  WHERE f2 IN topForums
  RETURN person, count(DISTINCT m) AS messageCount
  UNION ALL
  WITH topForums UNWIND topForums AS f
  MATCH (person)<-[:HAS_MEMBER]-(f:Forum) RETURN person, 0 AS messageCount
}
RETURN person.id, …, sum(messageCount) AS messageCount ORDER BY … LIMIT 100
```

## Two related gaps (either one unblocks it)
1. **`CALL { … }` subquery — unsupported** (parse `error`). This is the openCypher spelling.
2. **Composed UNION then continue — unsupported.** The GQL-native way to write the same thing is
   `{ <query1> UNION ALL <query2> } NEXT RETURN …, sum(messageCount) …`. go-gql rejects a **braced or
   parenthesized union used as a leading subquery**: `{ Q1 UNION Q2 } NEXT …` and `( Q1 UNION Q2 ) NEXT …`
   both `error`, and a bare `Q1 UNION Q2 NEXT Q3` parses as `Q1 UNION (Q2 NEXT Q3)` (wrong precedence,
   column-count mismatch). So a UNION result can't be fed into a further `NEXT` stage.

## Ask
Support **either** `CALL { <linear query> }` **or** a **union used as a composable subquery**
(`{ Q1 UNION [ALL] Q2 } NEXT …` / parenthesized), so a UNION's rows can flow into a subsequent aggregating
stage. The second is the more GQL-idiomatic fix and also helps any "combine two sources then aggregate"
query. Repro: author `gql/bi/q4.gql` (carries a `-- blocked:` header pointing here).

## Disposition (gochickpeas session, 2026-07-06): already supported -- option 1 shipped earlier today

`CALL { ... }` landed with the streaming-executor commit (56ce1a9), evidently after this task's probe.
Q4's full mechanics verified empirically through the public API -- imported collected list, `FOR` over it
inside the CALL, per-branch aggregation, `UNION ALL`, outer `sum`:

```
MATCH (p:Person) RETURN collect(p) AS people
NEXT CALL (people) {
  FOR p IN people
  MATCH (p)-[:CREATED]->(m:Post)
  RETURN p AS person, count(DISTINCT m) AS mc
  UNION ALL
  FOR p IN people
  RETURN p AS person, 0 AS mc
}
RETURN count(person) AS rows, sum(mc) AS total
```

Two spelling notes for authoring `gql/bi/q4.gql`:
- Imports use the GQL variable-scope clause `CALL (people) { ... }`, NOT Cypher's `CALL { WITH people ... }`
  (that spelling gets a targeted parse error pointing at the GQL forms).
- `WHERE f2 IN topForums` works with node values (uint32 entity identity comparison).

The braced/parenthesized composed-union form (`{ Q1 UNION ALL Q2 } NEXT ...`) was NOT added: ISO GQL
composes UNION at the top level only, and `CALL { Q1 UNION ALL Q2 }` is the conformant spelling of
"union then continue". See also task 042's engine disposition.
