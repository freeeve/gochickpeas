# gql -- the read-only GQL subset grammar

The authoritative spec for what `gql/internal/parser` accepts: a read-only
subset of ISO/IEC 39075:2024 (GQL), sized to the engine ported from
rustychickpeas-cypher. The Rust engine's `cypher.pest` is the feature
checklist; each construct below names its Cypher ancestor where the syntax
differs. Restrictions against full ISO GQL are marked **[restriction]**,
engine-specific extensions **[extension]**, and rejected-with-a-pointer
Cypher spellings **[cypher]**.

Notation: EBNF-ish; `[x]` optional, `x*` repetition, `|` alternation,
quoted text literal and case-insensitive. Whitespace separates tokens and
is otherwise insignificant. Comments: `//` to end of line, `/* ... */`
(non-nesting, must terminate).

## Lexical rules

```
ident   = (LETTER | '_') (LETTER | DIGIT | '_')*       -- unicode letters; not a reserved word
        | '`' (!'`' ANY)+ '`'                          -- delimited (spaces etc.); reserved
                                                       -- checks still apply by text [divergence:
                                                       -- ISO delimits with double quotes, which
                                                       -- are a string quote here]
integer = DIGIT+
float   = DIGIT+ '.' DIGIT+ [EXP] | DIGIT+ EXP         -- EXP = ('e'|'E') ['+'|'-'] DIGIT+;
                                                       -- 1..3 lexes as int '..' int; `1e` with no
                                                       -- digits stays int + ident
string  = "'" char* "'" | '"' char* '"'                -- quote doubling ('') and backslash
                                                       -- escapes: \\ \' \" \` \n \t \r \uXXXX;
                                                       -- an unknown escape is an error
param   = '$' identchar+                               -- name skips the reserved check ($end ok)
```

Keywords are case-insensitive. **Reserved words** (never identifiers):

    distinct optional return exists order limit match where false yield
    with when then else case call skip null true desc asc end and not as
    by or is in union reduce unwind
    filter let for next offset shortest
    insert set delete remove create drop merge detach session commit rollback

The last line is the write/catalog/session set: reserved solely so `INSERT
...` fails with "read-only engine" instead of a generic syntax error.
`starts`, `ends`, `contains`, `all`, `any`, `none`, `single`, `count`,
`explain`, `profile` are NOT reserved (usable as identifiers); the parser
recognizes them contextually, matching the Rust grammar.

Two-word operators (`STARTS WITH`, `ENDS WITH`, and the statement prefixes
`ANY SHORTEST` / `ALL SHORTEST`) are matched by token lookahead, so any
whitespace between the words is fine. Arrow punctuation is tokenized
loosely (`<-` is `<` `-`), so `< -[...]-` parses like `<-[...]-`
**[restriction: laxer than ISO]**.

## Queries

```
query        = ['EXPLAIN' | 'PROFILE'] part ('UNION' ['ALL'] part)*
part         = statement* return
statement    = match | filter | let | for | call | orderby | boundary
boundary     = return 'NEXT'                -- projection boundary between segments
return       = 'RETURN' ['DISTINCT'] item (',' item)*
               ['ORDER' 'BY' sort (',' sort)*] [('OFFSET'|'SKIP') integer]
               ['LIMIT' integer]
orderby      = 'ORDER' 'BY' sort (',' sort)* [('OFFSET'|'SKIP') integer]
               ['LIMIT' integer]             -- sort/cut the binding table
item         = '*' | expr ['AS' ident]
sort         = expr ['ASC' | 'DESC']
```

- `EXPLAIN`/`PROFILE` prefix is **[extension]** (no standard GQL explain).
- `RETURN ... NEXT` composes multi-part queries (GQL linear composition);
  it is exactly Cypher's `WITH`. `WITH` itself is rejected **[cypher]**.
- `OFFSET` is the GQL keyword; `SKIP` is accepted as a synonym.
- `ORDER BY` keys on a plain `RETURN` may reference the incoming
  variables; under `DISTINCT` or aggregation they must reference the
  projected columns (a key over a variable the projection discards is
  ambiguous per surviving row -- bind error, per ISO GQL/openCypher).
  To order by a discarded column, sort first and project in the next
  statement: `RETURN a, b ORDER BY b NEXT RETURN DISTINCT a LIMIT n`.
- The standalone `ORDER BY [OFFSET] [LIMIT]` statement sorts (and cuts)
  the binding table mid-pipeline -- it lowers to a star projection
  carrying only the ordering, the analogue of Cypher's
  `WITH * ORDER BY ... LIMIT n`. A downstream `collect`/`collect_list`
  aggregates rows in that order.
- UNION combines whole linear parts; `len(union) = len(parts) - 1`.

```
filter       = 'FILTER' expr                -- lowers to a pass-through projection + where
let          = 'LET' ident '=' expr (',' ident '=' expr)*
for          = 'FOR' ident 'IN' expr        -- list to rows (Cypher UNWIND [cypher])
```

`FOR ... WITH ORDINALITY` is not supported **[restriction]**.

## MATCH

```
match        = ['OPTIONAL'] 'MATCH' body ['WHERE' expr]
body         = pathsearch | pathbind | [mode] pattern (',' pattern)*
pathbind     = ident '=' [mode] pattern
pathsearch   = ident '=' ('ANY' | 'ALL') 'SHORTEST' pattern ['COST' expr]
mode         = 'TRAIL' | 'ACYCLIC'
```

- `ANY SHORTEST` binds the single hop-minimal path; `ALL SHORTEST` emits
  one row per minimal path. These replace Cypher's `shortestPath()` /
  `allShortestPaths()` **[cypher]**. `SHORTEST k` and selective-search
  prefixes beyond ANY/ALL are not supported **[restriction]**.
- Path modes: the engine's traversal semantics is TRAIL (no repeated
  relationship), so the `TRAIL` prefix is accepted as a no-op; `ACYCLIC`
  additionally forbids repeated nodes within each quantified segment (the
  start included) and requires a bounded quantifier with min >= 1. `WALK`
  (ISO's repeats-allowed default) and `SIMPLE` are rejected with targeted
  errors **[restriction]**; a path mode does not combine with ANY/ALL
  SHORTEST (the search normalizes the mode away).
- `COST expr` on `ANY SHORTEST` selects the minimum total-cost path
  instead of the hop-minimal one **[extension]** (ISO GQL has no weighted
  search; this drives the engine's Dijkstra kernel). A numeric literal is
  a constant per-edge weight; `r.prop` (the pattern's relationship
  variable) reads the property per edge; any other expression is a
  per-edge formula over that variable -- an edge whose weight is
  non-numeric, negative, or non-finite is excluded from the search.
  `ALL SHORTEST` does not combine with COST; the Cypher spellings
  `weightedShortestPath(...)` / `cost(shortestPath(...))` stay rejected
  with pointers here **[cypher]**.

## Patterns

```
pattern      = node (rel node)*
node         = '(' [ident] [':' labelexpr] [propmap] ')'
labelexpr    = labeland ('|' labeland)*
labeland     = labelnot (('&' | ':') labelnot)*
labelnot     = '!'* (ident | '(' labelexpr ')')
propmap      = '{' ident ':' expr (',' ident ':' expr)* '}'
rel          = arrow [quantifier]
arrow        = '-'  [detail] '->'            -- outgoing
             | '<-' [detail] '-'             -- incoming
             | '-'  [detail] '-'             -- undirected
detail       = '[' [ident] [':' ident ('|' ident)*] [propmap] ']'
quantifier   = '{' [integer] [',' [integer]] '}' | '*' | '+'
```

- Label expressions: `!` binds tighter than `&`/`:` (conjunction) which
  bind tighter than `|`. A plain conjunction flattens to the fast label
  list; anything else becomes a boolean label-expression predicate. The
  `%` any-label wildcard is not supported **[restriction]**.
- Inline property values are full expressions; a literal or `$param` keeps
  the index-seek fast path, anything else is desugared to a `WHERE`
  equality (M12).
- Quantifiers follow the arrow (GQL): `{m,n}`, `{m,}`, `{,n}`, `{m}`
  (exactly m), `*` = `{0,}`, `+` = `{1,}`. Cypher's in-bracket `*m..n` is
  rejected with a pointer to the quantifier form **[cypher]**. A
  quantifier applies to the single preceding relationship pattern --
  parenthesized/group quantification is not supported **[restriction]**.

## CALL

```
call         = 'CALL' '{' part ('UNION' ['ALL'] part)* '}'
             | 'CALL' '(' [ident (',' ident)*] ')' '{' ... '}'
             | 'CALL' procname '(' [expr (',' expr)*] ')'
               'YIELD' ident ['AS' ident] (',' ident ['AS' ident])*
procname     = ident ('.' ident)*
```

The `CALL (vars) { ... }` variable-scope clause is the GQL correlated
form (Cypher's leading `WITH vars` import **[cypher]**); the parser
records the imports AND synthesizes the importing projection into every
UNION branch of the body, which is the clause shape the binder consumes.

Procedure arguments are general expressions. Constant arguments (literals,
negated numbers, lists of constants) validate at plan time; any other
argument makes the call **correlated**: its arguments are checked against
the in-scope variables, evaluated per input row, and the procedure's rows
cross-join with that row. A row whose evaluated arguments fail the
procedure's validation yields no rows (total-eval semantics, like null
propagation); static mistakes -- unknown procedure, bad yield field, wrong
constant type -- are still plan errors. Node arguments accept a bound node
or an integer node id.

Procedures: `wcc`, `algo.bfs`, `algo.pagerank`, `algo.wcc`, `algo.cdlp`,
`algo.lcc`, `algo.sssp`, `fts.search`, `geo.withinRadius`,
`geo.withinBBox`, and `algo.propagate(seeds, values, relTypes, direction,
maxDepth, valueProp, order, truncLimit[, minValue[, filterProp, filterMin,
filterMax]]) YIELD node, value, depth` -- first-claim value propagation: a
depth-bounded BFS per seed where each expansion's rels (the relTypes
union, optionally range-filtered on an integer rel property) order by the
valueProp rel property ('asc'/'desc'), truncate to truncLimit (0 = none),
and the first rel to reach a node claims it, carrying that rel's value if
it exceeds minValue (default 0); across seeds a node sums its claimed
values and keeps its minimum depth (seeds are depth 1). This is the
stateful money-flow/taint-trace shape that pattern matching plus
aggregation cannot express.

## Expressions

Precedence, loosest to tightest (all infix left-associative):

| level | operators |
|---|---|
| 1 | `OR` |
| 2 | `AND` |
| 3 | `NOT` (prefix; looser than comparisons: `NOT a = b` is `NOT (a = b)`) |
| 4 | `=` `<>` `<` `<=` `>` `>=` `IN` `STARTS WITH` `ENDS WITH` `CONTAINS` |
| 5 | `+` `-` |
| 6 | `*` `/` |
| 7 | unary `-` |
| 8 | postfix: `IS [NOT] NULL`, slice `[a..b]` (tried before index), index `[e]`, `.key`, `:labelexpr` |

Primaries, with the ambiguity orderings that matter:

```
primary      = '(' expr ')' | list | listcomp | maplit | literal | param
             | case | existssub | countsub | listpred | cast | funccall
             | ident
list         = '[' [expr (',' expr)*] ']'
listcomp     = '[' ident 'IN' expr ['WHERE' expr] ['|' expr] ']'
maplit       = '{' [ident ':' expr (',' ident ':' expr)*] '}'
case         = 'CASE' [expr] ('WHEN' expr 'THEN' expr)+ ['ELSE' expr] 'END'
existssub    = 'EXISTS' '{' ['MATCH'] pattern ['WHERE' expr] '}'
countsub     = 'COUNT'  '{' ['MATCH'] pattern ['WHERE' expr] '}'
listpred     = ('all'|'any'|'none'|'single') '(' ident 'IN' expr 'WHERE' expr ')'
cast         = 'CAST' '(' expr 'AS' typename ')'
funccall     = ident '(' ['DISTINCT'] ('*' | expr (',' expr)*) ')'
literal      = integer | float | string | 'true' | 'false' | 'null'
```

- `COUNT { ... }` (brace) is the counting subquery; `count(...)` (paren)
  stays the aggregate. Same trick as the Rust grammar. The `MATCH`
  keyword inside either subquery body is optional (GQL's bare
  `EXISTS { <pattern> }` spelling).
- A quantifier name followed by `( ident IN` is a list predicate;
  otherwise it is an ordinary function call.
- The label predicate `:` applies only to a variable (`n:Comment`);
  `x IS [NOT] LABELED <labelexpr>` is the GQL spelling of the same
  predicate (postfix, desugared to it).
- List comprehensions `[x IN xs [WHERE p] [| m]]` are an extension with
  no ISO spelling **[extension]**: a leading `ident IN` after `[` always
  opens a comprehension (the Rust grammar's ordered choice); parenthesize
  a membership test to keep it a literal element. Pattern comprehensions
  stay rejected.
- `CAST(expr AS FLOAT|INTEGER|STRING|BOOLEAN)` lowers to the matching
  conversion function (`toFloat` etc.); other target types are rejected.
- `zoned_datetime` is a synonym of `datetime`, and `collect_list` of the
  `collect` aggregate (the GQL-flavored spellings the LDBC corpus uses).

## Excluded surface (engine Expr nodes exist; parser rejects)

Each of these parses in the Rust Cypher engine but has no ISO GQL
spelling; the AST keeps the node types so the engine layers port
unchanged, and adding syntax later is parser-only work.

| construct | rejection |
|---|---|
| pattern comprehension `[(a)-[]->(b) \| e]` | detected at the `WHERE`/`\|` |
| `reduce(acc = init, x IN xs \| e)` | targeted error |
| map projection `n{.key, .*}` | targeted error ('{' postfix) |
| bare pattern predicate in WHERE | write `EXISTS { MATCH ... }` |
| `cost(shortestPath(...), w)` | targeted error |
| `shortestPath()` family | pointer to `ANY/ALL SHORTEST` |
| `WITH`, `UNWIND`, `*m..n`, `SKIP`-only spelling | pointers to `RETURN...NEXT` / `LET` / `FILTER`, `FOR`, quantifiers (SKIP itself is accepted) |

## Deviations from ISO GQL worth knowing

- Strings have no escape sequences (byte-for-byte parity with the Rust
  engine's grammar); a quote character cannot appear in a string delimited
  by that quote.
- No comments, no session/catalog/transaction statements, no write
  statements (reserved keywords give the read-only error).
- Only linear path patterns (no parenthesized path patterns or
  graph-pattern `WHERE` inside the pattern; path modes are limited to
  TRAIL/ACYCLIC prefixes).
- `GROUP BY` is implicit (Cypher-style, by the non-aggregate projection
  keys); the explicit GQL `GROUP BY` clause is not accepted
  **[restriction]** -- revisit if the corpus needs it.

## Task 123 additions (GPML batch)

- Element patterns take an inline predicate: `(v:L {..} WHERE expr)` and
  `[r:T {..} WHERE expr]` -- desugared onto the clause WHERE (the ISO
  evaluation point), so the predicate may reference any clause variable.
  Not allowed on a variable-length relationship.
- `?` is the {0,1} quantifier.
- `%` in a label expression is the any-label wildcard (at least one
  label), composing with `&`, `|`, `!`, parentheses.
- MATCH modes parse: `DIFFERENT EDGES` (the engine default, a no-op) and
  `REPEATABLE ELEMENTS` (walk semantics: relationship uniqueness is not
  enforced within that clause).
- `%` in expression position is the modulo operator, multiplicative
  precedence, parse-time sugar for `mod(a, b)`.

## Task 124 additions (expression/composition batch)

- `XOR` at its ISO precedence (between OR and AND), three-valued.
- `||` concatenates strings and lists (additive precedence); it never
  adds numbers -- any other operand pair is null.
- `IS [NOT] TRUE | FALSE | UNKNOWN | TYPED <type>` postfix predicates.
  UNKNOWN is the null truth value (== IS NULL); TYPED covers INTEGER,
  FLOAT, STRING, BOOLEAN, LIST, NODE, RELATIONSHIP.
- Set operations: `UNION [ALL | DISTINCT]`, `EXCEPT [DISTINCT]`,
  `INTERSECT [DISTINCT]` -- EXCEPT/INTERSECT use distinct-set semantics.
- `VALUE { ... }` scalar subqueries are recognized and rejected with a
  targeted error (feature pending).
