# 013 -- gql parser (M11)

The GQL front-end (per the approved port plan):

- `gql/GRAMMAR.md`: the authoritative read-only ISO GQL subset spec, with
  every restriction/extension/rejected-Cypher-spelling documented.
- `gql/internal/ast`: language-neutral AST ported from the Rust crate's
  ast.rs (all Expr node kinds, incl. engine-only ones the parser doesn't
  emit yet) + the shared expression walker.
- `gql/internal/parser`: hand-written lexer + recursive descent + Pratt
  (zero deps). GQL forms lower into the Rust segment model: RETURN...NEXT
  -> With, LET -> star+items With, FILTER -> star+where With, FOR ->
  Unwind, CALL (vars) {..} -> CallSubquery with synthesized import With.
  Write keywords reserved for clean read-only errors.

Gate: parse.rs tests translated to GQL + GQL-form tests + FuzzParseGQL;
parser+ast coverage >=80%; `go test ./... -race` green.
