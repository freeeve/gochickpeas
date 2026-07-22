// Coverage for the label-expression arm of the decorrelation canonical
// identity: canonLabelExpr's structural encoding (exercised directly for
// every kind and its fail-closed propagation) and its reach through
// decorCanon's node encoder.
package compile

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func labelName(s string) *ast.LabelExpr { return &ast.LabelExpr{Kind: ast.LabelName, Name: s} }

// encLabel renders a label expression through canonLabelExpr.
func encLabel(e *ast.LabelExpr) (string, bool) {
	var sb strings.Builder
	ok := canonLabelExpr(&sb, e)
	return sb.String(), ok
}

// TestCanonLabelExpr covers every label-expression kind, deterministic and
// order-sensitive encoding, and the fail-closed propagation for an
// unrenderable subtree.
func TestCanonLabelExpr(t *testing.T) {
	if s, ok := encLabel(labelName("Person")); !ok || s != ":n(Person)" {
		t.Fatalf("name = %q,%v, want :n(Person)", s, ok)
	}
	person, _ := encLabel(labelName("Person"))
	if city, _ := encLabel(labelName("City")); city == person {
		t.Fatal("distinct labels must encode distinctly")
	}

	if s, ok := encLabel(&ast.LabelExpr{Kind: ast.LabelWild}); !ok || s != ":w" {
		t.Fatalf("wild = %q,%v, want :w", s, ok)
	}

	and := &ast.LabelExpr{Kind: ast.LabelAnd, L: labelName("A"), R: labelName("B")}
	or := &ast.LabelExpr{Kind: ast.LabelOr, L: labelName("A"), R: labelName("B")}
	sAnd, ok1 := encLabel(and)
	sOr, ok2 := encLabel(or)
	if !ok1 || !ok2 || sAnd == sOr {
		t.Fatalf("and/or must both encode and differ: %q vs %q", sAnd, sOr)
	}
	if !strings.Contains(sAnd, ":and(") || !strings.Contains(sOr, ":or(") {
		t.Fatalf("operator markers missing: %q / %q", sAnd, sOr)
	}
	if again, _ := encLabel(and); again != sAnd {
		t.Fatal("encoding must be deterministic")
	}
	andBA := &ast.LabelExpr{Kind: ast.LabelAnd, L: labelName("B"), R: labelName("A")}
	if s, _ := encLabel(andBA); s == sAnd {
		t.Fatal("operand order must be significant")
	}

	if s, ok := encLabel(&ast.LabelExpr{Kind: ast.LabelNot, L: labelName("A")}); !ok || !strings.Contains(s, ":not(") {
		t.Fatalf("not = %q,%v", s, ok)
	}

	// A nested tree encodes without incident.
	if _, ok := encLabel(&ast.LabelExpr{Kind: ast.LabelAnd, L: labelName("A"), R: or}); !ok {
		t.Fatal("nested label expr must encode")
	}

	// An unknown kind fails closed, and that failure propagates out of every
	// composite arm (either And operand, Or, and Not).
	bad := &ast.LabelExpr{Kind: ast.LabelKind(99)}
	if _, ok := encLabel(bad); ok {
		t.Fatal("unknown label kind must fail closed")
	}
	for name, e := range map[string]*ast.LabelExpr{
		"and-left":  {Kind: ast.LabelAnd, L: bad, R: labelName("A")},
		"and-right": {Kind: ast.LabelAnd, L: labelName("A"), R: bad},
		"or-right":  {Kind: ast.LabelOr, L: labelName("A"), R: bad},
		"not":       {Kind: ast.LabelNot, L: bad},
	} {
		if _, ok := encLabel(e); ok {
			t.Fatalf("failure must propagate through %s", name)
		}
	}
}

// TestDecorCanonWithLabelExpr covers the node encoder's label-expression
// branch: a pattern whose node carries a LabelExpr canonicalizes through it,
// distinct expressions yield distinct identities, and an unrenderable one
// fails closed to no identity.
func TestDecorCanonWithLabelExpr(t *testing.T) {
	pat := func(le *ast.LabelExpr) *ast.Pattern {
		return &ast.Pattern{Start: ast.NodePat{Var: "a", LabelExpr: le}}
	}
	person := decorCanon(pat(labelName("Person")), nil, "a", "")
	if person == "" {
		t.Fatal("a label-expr pattern must canonicalize")
	}
	if decorCanon(pat(labelName("City")), nil, "a", "") == person {
		t.Fatal("distinct label exprs must canonicalize distinctly")
	}
	if decorCanon(pat(labelName("Person")), nil, "a", "") != person {
		t.Fatal("identical shapes must canonicalize identically")
	}
	// A label expression the encoder cannot render yields no shared identity.
	if decorCanon(pat(&ast.LabelExpr{Kind: ast.LabelKind(99)}), nil, "a", "") != "" {
		t.Fatal("unrenderable label expr must yield no identity")
	}
}
