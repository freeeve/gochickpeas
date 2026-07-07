# 050 -- go-gql: BI Q4 top-100 selection DIFF -> ORDER BY-under-DISTINCT scope strictness

**Origin: the ldbc side's pure-GQL baseline (their tasks/295), 2026-07-06.** Their `gql/bi/q4.gql`
ran on go-gql but DIFFed on the top-100 forum selection; the `-- blocked:` header called it a
query-semantics detail. It is -- but go-gql was complicit by silently accepting the ambiguous form.

## Root cause

The official Cypher stages the selection:

```
WITH country, forum, count(person) AS numberOfMembers
ORDER BY numberOfMembers DESC, forum.id ASC, country.id
WITH DISTINCT forum
LIMIT 100
```

(sort the per-(country, forum) rows, dedupe forums first-occurrence, cut). Their GQL translation
collapsed it into one statement:

```
RETURN DISTINCT forum AS topForum ORDER BY numberOfMembers DESC, forum.id ASC, country.id LIMIT 100
```

A forum appears once per member-country, so sort keys over `numberOfMembers`/`country` -- variables
the DISTINCT projection discards -- are ambiguous per surviving row. Both ISO GQL and openCypher
reject this form. go-gql accepted it: `plan.BindProjection` kept the full incoming scope for ORDER BY
on any non-aggregating projection, and the executor's projSink dedupes first-occurrence then sorts by
keys evaluated against the first-kept duplicate's bindings (MATCH encounter order) -- a silently
arbitrary answer. Reproduced on sf1_canonical.rcpg: 99/100 rows off vs `python/refs/bi/q4.rust.json`.

## Fix (two halves)

1. **Query spelling (theirs)** -- stage it, mirroring the Cypher:

```
RETURN country, forum, numberOfMembers ORDER BY numberOfMembers DESC, forum.id ASC, country.id
NEXT
RETURN DISTINCT forum AS topForum LIMIT 100
```

Verified end-to-end on sf1_canonical.rcpg: **100/100 rows exact vs the rust ref** (positional,
totally-ordered tail), ~7s warm including the CALL{UNION ALL} message-count phase. go-gql's DISTINCT
is documented first-occurrence and its sort stable, so dedupe-then-LIMIT after the sorted boundary
reproduces the Cypher semantics. Ping with the drop-in text appended to their tasks/295.

2. **Engine strictness (ours)** -- `RETURN DISTINCT ... ORDER BY <discarded var>` is now a bind
error (`ORDER BY under DISTINCT must reference a projection column`), same treatment aggregation
already had; keys over projected columns/aliases stay legal. proj_bind.go + cover_test.go +
GRAMMAR.md note. The lax form would have kept producing plausible-but-arbitrary answers for any
query with this shape.

## Verification

gofmt -s clean; go test ./gql/... green; FuzzQuery 45s (2.09M execs) clean; parity gate
**87/87 MATCH, 0 DIFF, 0 SKIP** (manifest grew 78 -> 87 since 047); public-API drive: rejected forms
give clean bind errors, alias/projected-column keys and non-DISTINCT wide scope unchanged.
