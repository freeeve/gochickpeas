package plan

import (
	"math"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestPropSel covers the property-selectivity estimate: selectivity is 0.1
// per concrete property value, parameters and null do not count, and the
// product is floored at the smallest positive float rather than underflowing
// to zero.
func TestPropSel(t *testing.T) {
	pe := func(l ast.Literal) ast.PropEntry { return ast.PropEntry{Key: "k", Val: l} }

	if got := propSel(nil); got != 1 {
		t.Fatalf("propSel(nil) = %v, want 1", got)
	}
	if got := propSel([]ast.PropEntry{pe(ast.IntLit(1))}); got != 0.1 {
		t.Fatalf("one concrete = %v, want 0.1", got)
	}
	if got := propSel([]ast.PropEntry{pe(ast.IntLit(1)), pe(ast.StrLit("x"))}); math.Abs(got-0.01) > 1e-12 {
		t.Fatalf("two concrete = %v, want ~0.01", got)
	}
	// Parameters and null are not concrete, so only the one literal counts.
	mixed := []ast.PropEntry{
		pe(ast.IntLit(1)),
		pe(ast.Literal{Kind: ast.LitParam, P: 0}),
		pe(ast.Literal{Kind: ast.LitNamedParam, S: "p"}),
		pe(ast.NullLit()),
	}
	if got := propSel(mixed); got != 0.1 {
		t.Fatalf("one concrete among params/null = %v, want 0.1", got)
	}
	// Many concrete values underflow 0.1^n but are floored, not zero.
	many := make([]ast.PropEntry, 400)
	for i := range many {
		many[i] = pe(ast.IntLit(int64(i)))
	}
	if got := propSel(many); got != math.SmallestNonzeroFloat64 {
		t.Fatalf("underflow floor = %v, want the smallest positive float", got)
	}
}

// TestIsConcrete covers the concreteness predicate driving propSel: literal
// values are concrete, while parameters and null are not.
func TestIsConcrete(t *testing.T) {
	concrete := []ast.Literal{ast.IntLit(1), ast.FloatLit(1.5), ast.StrLit("x"), {Kind: ast.LitBool, B: true}}
	for _, l := range concrete {
		if !isConcrete(l) {
			t.Fatalf("%+v should be concrete", l)
		}
	}
	abstract := []ast.Literal{{Kind: ast.LitParam}, {Kind: ast.LitNamedParam}, ast.NullLit()}
	for _, l := range abstract {
		if isConcrete(l) {
			t.Fatalf("%+v should not be concrete", l)
		}
	}
}
