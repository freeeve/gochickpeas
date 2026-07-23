package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestPropEqSide covers the varName.key = <literal> matcher: it returns the
// key and seekable literal for a property on the target variable against a
// concrete or parameter literal, and declines a mismatched variable, a
// non-property or non-literal side, and a null literal (= null never seeks).
func TestPropEqSide(t *testing.T) {
	prop := func(v, k string) ast.Expr { return &ast.Prop{Var: v, Key: k} }

	// A matching property against a concrete literal returns (key, value).
	k, lit, ok := propEqSide(prop("n", "age"), &ast.Lit{Value: ast.IntLit(30)}, "n")
	if !ok || k != "age" || lit.Kind != ast.LitInt || lit.I != 30 {
		t.Fatalf("n.age = 30 -> (%q, %+v, %v)", k, lit, ok)
	}
	// A parameter literal seeks too (it abstains from costing, not matching).
	if _, _, ok := propEqSide(prop("n", "age"), &ast.Lit{Value: ast.Literal{Kind: ast.LitParam, P: 0}}, "n"); !ok {
		t.Fatal("a parameter literal must seek")
	}

	// Declines: a property on a different variable, a non-property or
	// non-literal side, and a null literal.
	if _, _, ok := propEqSide(prop("m", "age"), &ast.Lit{Value: ast.IntLit(1)}, "n"); ok {
		t.Fatal("a property on a different variable must decline")
	}
	if _, _, ok := propEqSide(&ast.Var{Name: "n"}, &ast.Lit{Value: ast.IntLit(1)}, "n"); ok {
		t.Fatal("a non-property side must decline")
	}
	if _, _, ok := propEqSide(prop("n", "age"), &ast.Var{Name: "x"}, "n"); ok {
		t.Fatal("a non-literal side must decline")
	}
	if _, _, ok := propEqSide(prop("n", "age"), &ast.Lit{Value: ast.NullLit()}, "n"); ok {
		t.Fatal("= null must not seek")
	}
}
