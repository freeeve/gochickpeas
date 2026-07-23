package plan

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
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

// TestNodeCardAndResolveByProps covers the leaf-cardinality and property-seek
// estimators against a fixture: without labels nodeCard is the total node
// count, a label alone gives its cardinality, and a concrete property narrows
// to the seek count (a parameter falls back to the label count); resolveByProps
// returns the seek ids only for a label plus a concrete property.
func TestNodeCardAndResolveByProps(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	us, err := b.AddNode("Person")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(us, "country", "US"); err != nil {
		t.Fatal(err)
	}
	ca, _ := b.AddNode("Person")
	if err := b.SetProp(ca, "country", "CA"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNode("City"); err != nil {
		t.Fatal(err)
	}
	g := graph.New(b.Finalize("country"))

	usProp := []ast.PropEntry{{Key: "country", Val: ast.StrLit("US")}}
	paramProp := []ast.PropEntry{{Key: "country", Val: ast.Literal{Kind: ast.LitParam, P: 0}}}

	// nodeCard: no labels -> all nodes; a label alone -> its cardinality; a
	// concrete property -> the seek count; a parameter -> back to the label.
	if got := nodeCard(nil, nil, g); got != 3 {
		t.Fatalf("nodeCard(no labels) = %d, want 3", got)
	}
	if got := nodeCard([]string{"Person"}, nil, g); got != 2 {
		t.Fatalf("nodeCard(Person) = %d, want 2", got)
	}
	if got := nodeCard([]string{"Person"}, usProp, g); got != 1 {
		t.Fatalf("nodeCard(Person {country:US}) = %d, want 1", got)
	}
	if got := nodeCard([]string{"Person"}, paramProp, g); got != 2 {
		t.Fatalf("nodeCard(Person {country:$p}) = %d, want 2", got)
	}

	// resolveByProps: no labels or only a parameter declines; a concrete
	// property returns exactly the seek ids.
	if _, ok := resolveByProps(nil, usProp, g); ok {
		t.Fatal("resolveByProps with no labels must decline")
	}
	if _, ok := resolveByProps([]string{"Person"}, paramProp, g); ok {
		t.Fatal("resolveByProps with only a parameter prop must decline")
	}
	ids, ok := resolveByProps([]string{"Person"}, usProp, g)
	if !ok || len(ids) != 1 || uint32(ids[0]) != uint32(us) {
		t.Fatalf("resolveByProps(Person {country:US}) = %v,%v, want [us],true", ids, ok)
	}
}
