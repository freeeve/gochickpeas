# gql -- ISO GQL conformance matrix

What the engine supports of ISO/IEC 39075:2024 (GQL), audited construct by
construct through the public API. Statuses are pinned by the conformance
corpus (`gql/conformance_test.go`, 184 probes): a construct changing
category fails the suite, so this matrix stays honest. GRAMMAR.md is the
authoritative subset grammar; this file is the gap analysis against full
ISO GQL. Legend:

- **OK** -- parses, plans, executes, verified result
- **reject** -- unsupported, clean error naming the construct
- **divergent** -- accepted with semantics that differ from ISO GQL

## Query composition

| Construct | Status | Notes |
|---|---|---|
| MATCH / OPTIONAL MATCH / multiple MATCH | OK | |
| MATCH modes (REPEATABLE ELEMENTS, DIFFERENT EDGES) | OK | DIFFERENT EDGES = default; REPEATABLE ELEMENTS = walk semantics per clause |
| FILTER expr | OK | `FILTER WHERE` form rejects |
| LET x = e [, ...] | OK | |
| FOR x IN list | OK | WITH ORDINALITY / WITH OFFSET reject |
| standalone ORDER BY / OFFSET / SKIP / LIMIT | OK | |
| RETURN [DISTINCT] items / * / AS / ORDER BY tail | OK | |
| RETURN ... NEXT composition | OK | |
| UNION / UNION ALL | OK | branches must alias to the same column names |
| UNION DISTINCT keyword | OK | the default, spelled explicitly |
| EXCEPT / INTERSECT | OK | distinct-set semantics |
| OTHERWISE | reject | |
| CALL { subquery }, CALL (imports) { } | OK | scope list is authoritative for every UNION branch; CALL () { } imports nothing (outer refs inside are bind errors) |
| CALL proc(...) YIELD | OK | fixed registry: wcc, algo.*, fts.search, geo.* |
| USE graph | reject | single-graph embedded engine |
| EXPLAIN / PROFILE prefix | OK | |
| WITH / UNWIND (Cypher) | reject | pointer errors name the GQL spelling |

## Graph patterns (GPML)

| Construct | Status | Notes |
|---|---|---|
| node patterns: (), (v), (:L), props, combinations | OK | |
| WHERE inside node/rel element pattern | OK | desugars to the clause WHERE |
| label expressions \| & ! ( ) and : conjunction | OK | |
| % label wildcard | OK | at least one label; composes with & ! ( ) |
| edge patterns -[]->, <-[]-, -[]- (+ abbreviated) | OK | |
| undirected ~[]~ family, <-[]-> any-directed | reject | -[]- covers the both-directions case |
| rel type alternation :A\|B | OK | |
| rel type negation :!A | reject | |
| inline rel properties {k: v} | reject | Tier 1: use a WHERE conjunct |
| quantifiers {m,n} {m,} {,n} {m} * + ? | OK | see multiplicity note below |
| quantified parenthesized path patterns | reject | patterns are linear node (rel node)* |
| named paths p = ... | OK | single-hop or bounded quantified only |
| path modes TRAIL / ACYCLIC | OK | TRAIL is the engine default |
| path modes WALK / SIMPLE | reject | |
| ANY SHORTEST / ALL SHORTEST [COST expr] | OK | endpoints must be pre-bound (Tier 1) |
| bare SHORTEST / SHORTEST k [GROUP] / ANY k | reject | |
| comma-separated patterns, cross-pattern equijoin | OK | |

**Multiplicity divergence:** a quantified pattern with min 0 or an
unbounded max resolves the *distinct reachable set* (dedup'd BFS), not the
per-trail multiset -- `count(*)` over `{1,}` counts endpoints where `{1,3}`
counts trails. Documented in GRAMMAR.md; a relationship variable on such a
pattern rejects for the same reason.

## Expressions

| Construct | Status | Notes |
|---|---|---|
| arithmetic + - * / % (precedence, unary -) | OK | % sugars to mod(); `^` rejects |
| string/list concat: `\|\|` (ISO) and + | OK | `\|\|` never adds numbers |
| comparisons = <> < <= > >= | OK | |
| AND / OR / NOT / XOR | OK | XOR is three-valued (null-propagating) |
| IS [NOT] NULL / TRUE / FALSE / UNKNOWN | OK | UNKNOWN == the null truth value |
| IS [NOT] LABELED, :Label postfix, IS [NOT] TYPED | OK | TYPED covers INTEGER/FLOAT/STRING/BOOLEAN/LIST/NODE/RELATIONSHIP |
| IN list | OK | |
| STARTS WITH / ENDS WITH / CONTAINS | OK | regex `=~` rejects |
| EXISTS { pattern }, COUNT { pattern } | OK | |
| VALUE { subquery } | reject | clean targeted error; feature pending |
| SAME / ALL_DIFFERENT / PROPERTY_EXISTS | reject | unknown function |
| CASE (searched + simple) | OK | |
| CAST to INT/FLOAT/STRING/BOOL/DATE/DATETIME/DURATION | OK | lowers to the constructor functions |
| COALESCE, NULLIF | OK | |
| $name parameters | OK | positional parameters reject |
| list literal / index / slice / comprehension | OK | |
| all/any/none/single(x IN l WHERE p) | OK | |
| map literal {k: v} | OK | |
| pattern comprehension / reduce / map projection | reject | pointer errors suggest rewrites |
| temporal literals DATE/DATETIME/TIMESTAMP/DURATION '...' | OK | lower to the constructor functions |
| scientific-notation floats (1.5e2, 2E-1) | OK | |
| string escapes ('' doubling, backslash incl. \\uXXXX) | OK | unknown escapes error |
| comments // and /* */ | OK | block comments must terminate |

## Functions

| Group | Status | Notes |
|---|---|---|
| count(*) / count / sum / avg / min / max / collect (+ DISTINCT) | OK | collect_list alias too |
| stddev_samp / stddev_pop (+ DISTINCT) | OK | Welford; 0 on empty/single, matching Neo4j |
| percentile_* | reject | two-arg aggregate machinery pending |
| size, substring, left, right, upper, lower | OK | |
| char_length, cardinality, trim, ltrim, rtrim | OK | char_length counts runes |
| normalize | reject | needs unicode tables (x/text); deliberate |
| abs, ceil, floor, round, sign, sqrt | OK | |
| mod, power, exp, ln, log10, sin/cos/tan/asin/acos/atan, degrees, radians | OK | non-finite results fold to null |
| toInteger, toFloat, toString, toBoolean, coalesce, range | OK | |
| id, type, startNode, endNode, nodes, relationships, length | OK | |
| element_id, labels, head, last, tail | OK | |
| properties | reject | needs a column-enumeration engine API; deliberate |
| date, datetime, localdatetime, duration constructors | OK | date() is a real Temporal(Date): midnight-truncated, accepts string/int/temporal/map |
| temporal component access (.year .month .day .hour ...) | OK | on date, datetime, and localdatetime |

The YYYYMMDD-integer `date()` (whose component access misread the value
as epoch-millis and whose int/temporal arguments were NULL -- BI Q16's
own spelling) was retired: `date()` now builds a midnight-truncated
Temporal(Date), converging with the Rust engine's model. Q16's empty SF1
reference was verified DATA-TRUE on the Rust side, so the migration
landed against existing references. Duration components follow the Neo4j
group convention (plural accessors); temporal accessors stay singular.

## Write / DDL / session surface

All write, catalog, and session statements reject with a "read-only
engine" error naming the keyword: INSERT, SET, DELETE, DETACH DELETE,
CREATE (incl. CREATE GRAPH), MERGE, DROP, REMOVE, SESSION, COMMIT,
ROLLBACK.

## Robustness (audited)

Zero panics across the corpus, including 200-deep nesting, 300-term
boolean chains, unterminated strings, overflow literals, empty input, and
near-miss keywords. Division by zero returns null. Unicode string
literals, unicode identifiers, and backtick-delimited identifiers all
work (ISO delimits identifiers with double quotes -- a string quote here,
so backtick is the divergent spelling; reserved words stay reserved even
delimited).
