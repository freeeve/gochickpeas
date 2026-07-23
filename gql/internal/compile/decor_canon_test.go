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

// encLit renders a literal through canonLiteral.
func encLit(l ast.Literal) string {
	var sb strings.Builder
	canonLiteral(&sb, l)
	return sb.String()
}

// encExpr renders a WHERE expression through canonExpr.
func encExpr(e ast.Expr, sub map[string]string) (string, bool) {
	var sb strings.Builder
	ok := canonExpr(&sb, e, sub)
	return sb.String(), ok
}

// TestCanonLiteralKinds pins that every literal kind encodes to a distinct,
// deterministic marker (distinct identities depend on it -- two subqueries
// differing only in a literal must not share a side table).
func TestCanonLiteralKinds(t *testing.T) {
	lits := map[string]ast.Literal{
		"int":     ast.IntLit(5),
		"int2":    ast.IntLit(6),
		"float":   ast.FloatLit(5),
		"str":     ast.StrLit("5"),
		"b:true":  {Kind: ast.LitBool, B: true},
		"b:false": {Kind: ast.LitBool, B: false},
		"param":   {Kind: ast.LitParam, P: 5},
		"named":   {Kind: ast.LitNamedParam, S: "x"},
		"null":    {Kind: ast.LitKind(200)},
	}
	seen := map[string]string{}
	for name, l := range lits {
		s := encLit(l)
		if prev, dup := seen[s]; dup {
			t.Fatalf("literal %s and %s collide on %q", name, prev, s)
		}
		seen[s] = name
		if again := encLit(l); again != s {
			t.Fatalf("literal %s encoding not deterministic", name)
		}
	}
	// int 5 and param slot 5 must not collide despite the shared number.
	if encLit(ast.IntLit(5)) == encLit(ast.Literal{Kind: ast.LitParam, P: 5}) {
		t.Fatal("int and param must encode distinctly")
	}
}

// TestCanonExprArms exercises the WHERE-expression encoder: each arm encodes
// and distinct expressions stay distinct, count(DISTINCT/*) and unhandled
// constructs fail closed, and a failing child aborts its enclosing node.
func TestCanonExprArms(t *testing.T) {
	sub := map[string]string{"a": "\x01"}
	v := func(name string) ast.Expr { return &ast.Var{Name: name} }
	lit := func(i int64) ast.Expr { return &ast.Lit{Value: ast.IntLit(i)} }

	// A representative expression per arm; all must encode, all distinct.
	arms := map[string]ast.Expr{
		"lit":    lit(1),
		"var":    v("b"),
		"prop":   &ast.Prop{Var: "b", Key: "id"},
		"propOf": &ast.PropOf{Base: v("b"), Key: "id"},
		"unary":  &ast.Unary{Op: ast.UnOp(0), Expr: v("b")},
		"binary": &ast.Binary{Op: ast.BinOp(0), LHS: v("b"), RHS: lit(1)},
		"isnull": &ast.IsNull{Expr: v("b")},
		"in":     &ast.In{Expr: v("b"), List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}}},
		"list":   &ast.ListExpr{Elems: []ast.Expr{lit(1), lit(2)}},
		"func":   &ast.Func{Name: "abs", Args: []ast.Expr{v("b")}},
		"case":   &ast.Case{Whens: []ast.CaseWhen{{Cond: v("b"), Result: lit(1)}}},
	}
	seen := map[string]string{}
	for name, e := range arms {
		s, ok := encExpr(e, sub)
		if !ok {
			t.Fatalf("arm %s failed to encode", name)
		}
		if prev, dup := seen[s]; dup {
			t.Fatalf("arm %s collides with %s on %q", name, prev, s)
		}
		seen[s] = name
		if again, _ := encExpr(e, sub); again != s {
			t.Fatalf("arm %s not deterministic", name)
		}
	}

	// IS NULL vs IS NOT NULL over the same operand must differ.
	pos, _ := encExpr(&ast.IsNull{Expr: v("b")}, sub)
	neg, _ := encExpr(&ast.IsNull{Expr: v("b"), Negated: true}, sub)
	if pos == neg {
		t.Fatal("IS NULL and IS NOT NULL must encode distinctly")
	}

	// count(DISTINCT x) and count(*) are never in a decor WHERE: fail closed.
	if _, ok := encExpr(&ast.Func{Name: "count", Distinct: true, Args: []ast.Expr{v("b")}}, sub); ok {
		t.Fatal("DISTINCT func must fail closed")
	}
	if _, ok := encExpr(&ast.Func{Name: "count", Star: true}, sub); ok {
		t.Fatal("star func must fail closed")
	}
	// An unhandled construct (a scoped subquery) fails closed...
	if _, ok := encExpr(&ast.Exists{}, sub); ok {
		t.Fatal("Exists must fail closed")
	}
	// ...and that failure propagates out through an enclosing node.
	if _, ok := encExpr(&ast.Binary{Op: ast.BinOp(0), LHS: &ast.Exists{}, RHS: lit(1)}, sub); ok {
		t.Fatal("a failing child must abort the enclosing Binary")
	}
}

// TestDecorCanonWhereSubstitution pins the identity contract end-to-end: two
// WHEREs differing only in the anchor variable's name share one identity
// (the whole point -- BI Q8's C1(person)/C1(friend)), while a semantically
// different predicate gets its own, and an unrenderable WHERE yields none.
func TestDecorCanonWhereSubstitution(t *testing.T) {
	pat := func(anchor string) *ast.Pattern {
		return &ast.Pattern{Start: ast.NodePat{Var: anchor}}
	}
	// person.id > 0 with anchor "person" vs friend.id > 0 with anchor "friend".
	gt := func(anchor string) ast.Expr {
		return &ast.Binary{Op: ast.BinOp(0), LHS: &ast.Prop{Var: anchor, Key: "id"}, RHS: &ast.Lit{Value: ast.IntLit(0)}}
	}
	person := decorCanon(pat("person"), gt("person"), "person", "")
	friend := decorCanon(pat("friend"), gt("friend"), "friend", "")
	if person == "" || person != friend {
		t.Fatalf("anchor-renamed predicates must share identity: %q vs %q", person, friend)
	}
	// A different property is a different predicate.
	other := &ast.Binary{Op: ast.BinOp(0), LHS: &ast.Prop{Var: "person", Key: "name"}, RHS: &ast.Lit{Value: ast.IntLit(0)}}
	if decorCanon(pat("person"), other, "person", "") == person {
		t.Fatal("a different predicate must not share identity")
	}
	// An unrenderable WHERE yields no identity (private table, not a wrong share).
	if decorCanon(pat("person"), &ast.Exists{}, "person", "") != "" {
		t.Fatal("unrenderable WHERE must yield no identity")
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
