# 048 — go-gql: tokenized/word-boundary text search for SPB FTS (a20-a23)

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** Follow-on to 043.
`lower()/upper()` (52cfabb) unblocked the SPB FTS queries whose keyword has no substring-superset in the
data -- **a15/a16 ('policy') now MATCH**. But the **'football' queries (a20, a21, a22, a23) DIFF**: the SPB
ref uses tokenized full-text search (`fts.search('title','football')`), which matches the *word* football,
while `lower(w.title) CONTAINS 'football'` matches the *substring* -- so it also picks up 'footballer',
'footballing', etc.

## Symptom
```
MATCH (w:CreativeWork) FILTER lower(w.title) CONTAINS 'football' AND w.dateModified IS NOT NULL RETURN w.uri
  -> 393 rows; the tokenized-FTS ref has 351 (the extra ~42 are 'footballer'/'footballing' titles)
```
There is no regex / `=~` / word-boundary operator in go-gql to approximate it either (checked).

## Ask
A word-boundary/tokenized text match usable in `FILTER` -- either a real FTS predicate
(`fts(w.title, 'football')`) or a regex-match operator (`w.title =~ '(?i)\bfootball\b'`) -- so the keyword
matches whole words. Unblocks `gql/spb/a20.gql`, `a21`, `a22`, `a23` (each carries a `-- blocked:` header
pointing here). a15/a16 already pass with plain `lower()+CONTAINS`, so this is only needed for keywords
that are substrings of other words.

## Disposition (gochickpeas session, 2026-07-06): already supported -- use CALL fts.search

go-gql already has the tokenized form, and it is the FAITHFUL translation of the original SPB
`CALL fts.search(...)`:

```
CALL fts.search('CreativeWork', 'title', 'football') YIELD node AS w
FILTER w.dateModified IS NOT NULL
RETURN w.uri AS uri
```

Verified empirically on spb_canonical.rcpg: this returns **351** rows (the tokenized-FTS ref count) vs
the substring form's 393. No index setup is needed -- the engine builds the (label, field) inverted
full-text index lazily on the first fts.search call (~0.2s on SPB, cached on the snapshot afterwards),
using the same tokenizer the rust reference used. The YIELD binds nodes, so further MATCH/FILTER clauses
compose for the a21-a23 join shapes. No engine change required; rewrite a20-a23 to the CALL form.

## Update (gochickpeas session, 2026-07-06): all four shapes verified against the rust refs

The disposition above only ran a20's flat shape. Now verified end-to-end on spb_canonical.rcpg -- the
CALL-form rewrite of each blocked query matches its `python/refs/spb/*.rust.json` result multiset exactly:

| query | shape after the YIELD | rows (= ref) |
|---|---|---|
| a20 | FILTER on property | 351 |
| a21 | FILTER with EXISTS{} subqueries (incl. OR of two) | 227 |
| a22 | date window + IS NOT NULL + four EXISTS{} | 108 |
| a23 | FILTER + MATCH join + count(DISTINCT substring) + NEXT | 2597 |

Drop-in texts (replace the `MATCH (w:CreativeWork) FILTER lower(w.title) CONTAINS 'football'` head with
the CALL, keep everything else; a20-a22 shown head-only, a23 in full since its tail follows the CALL too):

```
CALL fts.search('CreativeWork', 'title', 'football') YIELD node AS w
FILTER <rest of the existing FILTER, minus the lower()/CONTAINS conjunct>
RETURN w.uri AS uri
```

a23 in full:

```
CALL fts.search('CreativeWork', 'title', 'football') YIELD node AS w
FILTER EXISTS { MATCH (w)-[:category]->({uri:'http://www.bbc.co.uk/category/Event'}) }
  AND w.dateCreated IS NOT NULL AND w.description IS NOT NULL AND w.liveCoverage IS NOT NULL
  AND EXISTS { MATCH (w)-[:audience]->() } AND EXISTS { MATCH (w)-[:primaryFormat]->() }
MATCH (w)-[:tag]->(topic)
RETURN topic, count(DISTINCT substring(w.dateCreated, 0, 10)) AS days
NEXT
RETURN topic.uri AS k, days
```

Note for the baseline's portability goal (their tasks/295): `CALL procname(...)` is standard GQL syntax
and fts.search is the procedure the original SPB reference itself calls, so the CALL form is the faithful
translation, not an engine-specific workaround. Ping appended to rustychickpeas-ldbc tasks/295
(uncommitted).
