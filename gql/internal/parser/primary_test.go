// Primary-expression parser tests: the NORMALIZE bare-form lowering, the
// COLLECT { } list subquery, and the pattern-comprehension rejection.
package parser

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestNormalizeFuncLowersBareForm covers isNormForm: NORMALIZE / is_normalized
// take the Unicode normalization form as a bare keyword, which parses as a
// variable and is lowered to an upper-cased string literal. A non-form second
// argument is a genuine variable and stays one.
func TestNormalizeFuncLowersBareForm(t *testing.T) {
	formArg := func(src string) ast.Expr {
		t.Helper()
		f, ok := retExpr(t, src).(*ast.Func)
		if !ok || len(f.Args) != 2 {
			t.Fatalf("%s did not parse to a 2-arg call: %#v", src, retExpr(t, src))
		}
		return f.Args[1]
	}

	// A bare form keyword lowers to its upper-cased string literal.
	for _, c := range []struct{ src, want string }{
		{"RETURN normalize(s, NFC)", "NFC"},
		{"RETURN normalize(s, nfkd)", "NFKD"},
		{"RETURN is_normalized(s, NFD)", "NFD"},
	} {
		lit, ok := formArg(c.src).(*ast.Lit)
		if !ok || lit.Value.S != c.want {
			t.Fatalf("%s form arg = %#v, want Lit(%q)", c.src, formArg(c.src), c.want)
		}
	}

	// An already-quoted form is left as the string literal it already is.
	if lit, ok := formArg("RETURN normalize(s, 'NFC')").(*ast.Lit); !ok || lit.Value.S != "NFC" {
		t.Fatalf("quoted form = %#v, want Lit(NFC)", formArg("RETURN normalize(s, 'NFC')"))
	}
	// A second argument that is not a form keyword is a real variable.
	if _, ok := formArg("RETURN normalize(s, other)").(*ast.Var); !ok {
		t.Fatalf("non-form arg = %#v, want a Var", formArg("RETURN normalize(s, other)"))
	}
}

// TestCollectSubqueryParses covers parseBracedCollect: COLLECT { [MATCH]
// pattern [WHERE] RETURN proj [AS alias] } is the list-subquery brace form and
// parses to a PatternComp, distinct from the collect(...) aggregate call.
func TestCollectSubqueryParses(t *testing.T) {
	pc, ok := retExpr(t, "RETURN collect { MATCH (a)-[:R]->(b) WHERE b.x > 1 RETURN b }").(*ast.PatternComp)
	if !ok {
		t.Fatalf("COLLECT { } -> %#v, want *ast.PatternComp", retExpr(t, "RETURN collect { MATCH (a)-[:R]->(b) RETURN b }"))
	}
	if pc.Pattern == nil || pc.Where == nil || pc.Proj == nil {
		t.Fatalf("COLLECT parts incomplete: %+v", pc)
	}
	// The MATCH keyword is optional and an AS alias is accepted (and ignored).
	if _, ok := retExpr(t, "RETURN collect { (a)-[:R]->(b) RETURN b AS gathered }").(*ast.PatternComp); !ok {
		t.Fatal("COLLECT without MATCH and with an AS alias must still parse")
	}
	// COLLECT demands a RETURN projection.
	if _, err := Parse("RETURN collect { MATCH (a)-[:R]->(b) }"); err == nil {
		t.Fatal("COLLECT without RETURN must be a parse error")
	}
	// collect(...) with parentheses is the aggregate call, not the subquery.
	if _, ok := retExpr(t, "RETURN collect(x)").(*ast.Func); !ok {
		t.Fatalf("collect(x) -> %#v, want an aggregate *ast.Func", retExpr(t, "RETURN collect(x)"))
	}
}

// TestPatternComprehensionRejected covers patternCompErr: the openCypher
// pattern-comprehension syntax [pattern | expr] is not in the GQL subset and
// must be rejected with the guidance error, not silently parsed.
func TestPatternComprehensionRejected(t *testing.T) {
	_, err := Parse("RETURN [(a)-[:R]->(b) | b]")
	if err == nil {
		t.Fatal("a pattern comprehension must be rejected")
	}
	if !strings.Contains(err.Error(), "pattern comprehension") {
		t.Fatalf("rejection message = %q, want it to name pattern comprehensions", err)
	}
}
