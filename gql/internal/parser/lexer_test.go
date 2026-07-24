// Lexer tests: tokenization paths the statement-level suite (all-ASCII,
// ported from the Rust parse.rs corpus) does not reach.
package parser

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestUnicodeIdentifier covers identStartRune: an identifier beginning with a
// non-ASCII Unicode letter (Greek lambda) must lex as one TokIdent so the whole
// name reaches the AST, and a Unicode letter mid-identifier (an accented Latin
// letter) must continue the token. GQL identifiers are Unicode letters, but the
// manifest corpus is all ASCII, leaving both paths otherwise unexercised.
func TestUnicodeIdentifier(t *testing.T) {
	// Unicode-letter START -> identStartRune.
	q := mustParse(t, "MATCH (λ:Person) RETURN λ.name AS n")
	m, ok := q.Parts[0].Clauses[0].(*ast.Match)
	if !ok {
		t.Fatalf("clause 0 = %#v, want *ast.Match", q.Parts[0].Clauses[0])
	}
	if got := m.Patterns[0].Start.Var; got != "λ" {
		t.Fatalf("start var = %q, want \"λ\"", got)
	}

	// Unicode letter CONTINUING an ASCII-started identifier.
	q2 := mustParse(t, "MATCH (café:Person) RETURN café.name AS n")
	m2 := q2.Parts[0].Clauses[0].(*ast.Match)
	if got := m2.Patterns[0].Start.Var; got != "café" {
		t.Fatalf("accented var = %q, want %q", got, "café")
	}
}
