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
