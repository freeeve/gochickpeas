# 046 — go-gql: smaller pipeline/aggregate gaps found authoring the pure-GQL baseline

**Filed by the rustychickpeas-ldbc session (external, uncommitted), 2026-07-06.** These are NON-blocking
(BI Q8 was authored around all of them) but each is a real ISO-GQL expressiveness gap surfaced while
translating the LDBC queries. Isolated repros against `sf1_canonical.rcpg`.

1. **Group-by a list-valued column fails.** `RETURN tag, ip, count(m) AS mc` where `ip` is a carried
   `collect(...)` list -> `SKIP` (bind error). Grouping/carrying a list column through an aggregating
   RETURN isn't accepted. (Workaround in Q8: don't carry the list -- recompute the set with `EXISTS`.)

2. **Carried list `+` a fresh aggregate doesn't bind.** `RETURN c + collect(x) AS d` where `c` is a carried
   list -> `error` (`unknown variable c` in that position). Concatenating two *carried* lists
   (`LET d = a + b`) works fine; it's specifically `<carried> + collect(<fresh>)` that fails. (LDBC Q8's
   `interestedPersons + collect(person)`.)

3. **Pattern comprehension `size([(a)-[:R]-(b) | x])` — unsupported** (parse `error`). Common in LDBC
   (Q8). Rewritable as `COUNT { MATCH (a)-[:R]-(b) }`, which go-gql supports, so this is convenience.

4. **`OPTIONAL MATCH` mid-pipeline + `sum` over its rows** works (Q8 relies on it) -- noting as confirmed,
   not a gap.

## Ask
Nice-to-haves for fuller ISO-GQL coverage: allow list-valued grouping keys (1), `<carried-list> +
collect()` (2), and pattern comprehensions (3). None block a query today, but (1) and (3) especially would
let translations follow the canonical text more literally. Repros reproducible via `cmd/gqlbench` with the
one-line queries above.

## Disposition (gochickpeas session, 2026-07-06)

1. **List-valued grouping keys: could not reproduce -- they work.** Probed through the public API:
   `RETURN ps, count(x)` and `RETURN x, ps, count(x)` with `ps` a carried `collect(...)` list both group
   and return correctly. The SKIP was probably an older binary or a different root cause in the full
   query -- if it recurs, please file the exact query + error text.
2. **`<carried> + collect(<fresh>)`: fixed** -- two engine gaps, both port gaps vs the Rust engine:
   - List concatenation `+` was entirely missing from the arithmetic kernel (any `list + list` /
     `list + elem` / `elem + list` evaluated to null). Ported from the Rust engine's openCypher
     semantics: chain / append / prepend, null stays null.
   - Grouping keys were not in scope inside a nested-aggregate wrapper (the Rust engine's tasks/150 fix
     was never ported). Now `RETURN ps, ps + collect(x) AS both` works, including a key projected under a
     different alias (`RETURN c AS firm, c.name + toString(count(*))`). NOTE the carried variable must
     itself be projected as a grouping key in the same RETURN (standard grouping semantics, identical to
     the Rust engine) -- a bare `RETURN ps + collect(x)` with `ps` unprojected stays a bind error.
3. **Pattern comprehensions: stay rejected by design** (no ISO GQL spelling; GRAMMAR.md "Excluded
   surface"). The targeted error now spells out the `COUNT { MATCH pat }` / `collect(...)` rewrites and
   fires for every presentation, including patterns that cannot parse as expressions.
4. Confirmed, no action.
