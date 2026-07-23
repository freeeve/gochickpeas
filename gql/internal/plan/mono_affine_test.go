// Affine/size offset matcher tests for the monotonic-pushdown analysis
// (mono.go): sizePlus, affineOffset, and asVarName. Split from mono_test.go
// to keep each file under the length limit.
package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func monoVar(n string) ast.Expr { return &ast.Var{Name: n} }
func monoLit(i int64) ast.Expr  { return &ast.Lit{Value: ast.IntLit(i)} }
func monoSize(x ast.Expr) ast.Expr {
	return &ast.Func{Name: "size", Args: []ast.Expr{x}}
}
func monoAdd(l, r ast.Expr) ast.Expr { return &ast.Binary{Op: ast.OpAdd, LHS: l, RHS: r} }
func monoSub(l, r ast.Expr) ast.Expr { return &ast.Binary{Op: ast.OpSub, LHS: l, RHS: r} }

// TestSizePlus covers the size(X)[+/-c] matcher: it returns the inner argument
// and the signed offset for size(X), size(X)+c, c+size(X), and size(X)-c, and
// declines every other shape (a bare variable, a non-size sum, the unmatched
// c-size(X) form, and a non-additive operator).
func TestSizePlus(t *testing.T) {
	matches := []struct {
		name string
		e    ast.Expr
		off  int64
	}{
		{"bare", monoSize(monoVar("p")), 0},
		{"size+c", monoAdd(monoSize(monoVar("p")), monoLit(3)), 3},
		{"c+size", monoAdd(monoLit(3), monoSize(monoVar("p"))), 3},
		{"size-c", monoSub(monoSize(monoVar("p")), monoLit(2)), -2},
	}
	for _, c := range matches {
		x, off, ok := sizePlus(c.e)
		if !ok || off != c.off || asVarName(x) != "p" {
			t.Fatalf("%s: sizePlus = (%v,%d,%v), want (p,%d,true)", c.name, x, off, ok, c.off)
		}
	}

	declines := []struct {
		name string
		e    ast.Expr
	}{
		{"bare var", monoVar("p")},
		{"non-size add", monoAdd(monoVar("p"), monoLit(1))},
		{"c-size unmatched", monoSub(monoLit(1), monoSize(monoVar("p")))},
		{"non-additive op", &ast.Binary{Op: ast.OpMul, LHS: monoSize(monoVar("p")), RHS: monoLit(2)}},
	}
	for _, c := range declines {
		if x, _, ok := sizePlus(c.e); ok {
			t.Fatalf("%s: sizePlus should decline, got %v", c.name, x)
		}
	}
}

// TestAffineOffset covers the <ivar>[+/-c] matcher relative to the iteration
// variable: it returns the signed offset for ivar, ivar+c, c+ivar, and
// ivar-c, and declines a different variable, the unmatched c-ivar form, and a
// non-variable leaf.
func TestAffineOffset(t *testing.T) {
	matches := []struct {
		name string
		e    ast.Expr
		off  int64
	}{
		{"bare i", monoVar("i"), 0},
		{"i+c", monoAdd(monoVar("i"), monoLit(5)), 5},
		{"c+i", monoAdd(monoLit(5), monoVar("i")), 5},
		{"i-c", monoSub(monoVar("i"), monoLit(2)), -2},
	}
	for _, c := range matches {
		if off, ok := affineOffset(c.e, "i"); !ok || off != c.off {
			t.Fatalf("%s: affineOffset = (%d,%v), want %d", c.name, off, ok, c.off)
		}
	}

	declines := []struct {
		name string
		e    ast.Expr
	}{
		{"other var", monoVar("j")},
		{"c-i unmatched", monoSub(monoLit(2), monoVar("i"))},
		{"other var + c", monoAdd(monoVar("j"), monoLit(1))},
		{"literal", monoLit(3)},
	}
	for _, c := range declines {
		if off, ok := affineOffset(c.e, "i"); ok {
			t.Fatalf("%s: affineOffset should decline, got %d", c.name, off)
		}
	}
}

// TestAsVarName covers the bare-variable-name accessor: a Var yields its name,
// anything else the empty string.
func TestAsVarName(t *testing.T) {
	if got := asVarName(monoVar("x")); got != "x" {
		t.Fatalf("asVarName(Var x) = %q, want x", got)
	}
	if got := asVarName(monoLit(1)); got != "" {
		t.Fatalf("asVarName(Lit) = %q, want empty", got)
	}
}
