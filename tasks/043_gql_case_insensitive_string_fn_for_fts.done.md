# 043 — go-gql: no `lower()` / case-insensitive string fn (blocks SPB FTS queries a15, a20)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** Building the faithful
pure-GQL baseline (`rustychickpeas-ldbc/gql/*`), the SPB full-text-search queries translate the LDBC
`CALL fts.search('CreativeWork','title', <kw>)` to a case-insensitive substring match on `title`. go-gql
has `CONTAINS` but **no case-folding function**, so the faithful form doesn't bind.

## Symptom

```
MATCH (w:CreativeWork) FILTER lower(w.title) CONTAINS 'football' AND w.dateModified IS NOT NULL
RETURN w.uri AS uri
  -> SKIP  unknown function `lower`: gql bind error
```
`toUpper` also errors `unknown function toUpper`. A case-sensitive `title CONTAINS 'football'` returns 83
rows vs the FTS ref's 351 — the gap is precisely case (`Football`, `FOOTBALL`, …), so a case-fold is what's
needed.

## Ask
Add a case-folding scalar string function — `lower(str)` (and ideally `upper(str)`) — bound in the GQL
function registry, usable in `FILTER`/`RETURN`. (ISO GQL's normative name is `lower`/`upper`; openCypher
spells them `toLower`/`toUpper` — either alias is fine as long as one binds.) That unblocks the faithful
`lower(w.title) CONTAINS <kw>` form.

## Scope note (not part of this task)
This makes the FTS queries a **case-insensitive substring** match, which is what the SPB data needs here
(the ref difference is purely case). True tokenized/stemmed full-text search is a separate, larger feature
and is NOT required for a15/a20 to pass — only case-folding is. Blocked queries: `gql/spb/a15.gql`,
`gql/spb/a20.gql` (both carry a `-- blocked:` header pointing here). They stay authored in faithful GQL so
they run the moment `lower()` lands — per the "file the engine gap, don't hack the query" principle.
