// Binder tables: aggregate/function recognition, derived column names,
// aggregate detection boundaries, and reference validation scoping.
package semantics

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func TestFunctionRecognition(t *testing.T) {
	for _, name := range []string{"count", "COUNT", "Sum", "avg", "min", "MAX", "collect"} {
		if !IsAggName(name) {
			t.Fatalf("%s is an aggregate", name)
		}
	}
	if IsAggName("size") || IsAggName("uncount") {
		t.Fatal("non-aggregates")
	}
	for _, name := range []string{"size", "toString", "COALESCE", "startNode", "endNode", "relationships"} {
		if !IsKnownFunction(name) {
			t.Fatalf("%s is a known function", name)
		}
	}
	if IsKnownFunction("toUpper") || IsKnownScalarFunc("count") {
		t.Fatal("unknown scalar / aggregate misclassified")
	}
}

func TestDerivedName(t *testing.T) {
	cases := []struct {
		e    ast.Expr
		want string
	}{
		{&ast.Var{Name: "a"}, "a"},
		{&ast.Prop{Var: "a", Key: "name"}, "a.name"},
		{&ast.Func{Name: "count", Star: true}, "count(*)"},
		{&ast.Func{Name: "size", Args: []ast.Expr{&ast.Prop{Var: "a", Key: "xs"}}}, "size(a.xs)"},
		{&ast.Func{Name: "substring", Args: []ast.Expr{&ast.Var{Name: "s"}, &ast.Lit{Value: ast.IntLit(1)}}}, "substring(s, expr)"},
		{&ast.Lit{Value: ast.IntLit(1)}, "expr"},
		{&ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "a"}, RHS: &ast.Var{Name: "b"}}, "expr"},
	}
	for _, c := range cases {
		if got := DerivedName(c.e); got != c.want {
			t.Fatalf("DerivedName = %q, want %q", got, c.want)
		}
	}
}

func TestExprHasAgg(t *testing.T) {
	agg := &ast.Func{Name: "count", Args: []ast.Expr{&ast.Var{Name: "m"}}}
	if !ExprHasAgg(agg) {
		t.Fatal("count(m)")
	}
	if !ExprHasAgg(&ast.Binary{Op: ast.OpAdd, LHS: &ast.Lit{Value: ast.IntLit(1)}, RHS: agg}) {
		t.Fatal("1 + count(m)")
	}
	// A computed map-literal field counts (the IC idiom).
	if !ExprHasAgg(&ast.MapLit{Fields: []ast.MapField{{Key: "cnt", Val: agg}}}) {
		t.Fatal("{cnt: count(m)}")
	}
	if !ExprHasAgg(&ast.MapProj{Var: "p", Entries: []ast.MapProjEntry{{Kind: ast.MapProjField, Key: "c", Expr: agg}}}) {
		t.Fatal("p{c: count(m)}")
	}
	// An aggregate inside an EXISTS subquery is a different scope: not
	// counted, mirroring the Rust binder.
	ex := &ast.Exists{Pattern: &ast.Pattern{}, Where: agg}
	if ExprHasAgg(ex) {
		t.Fatal("EXISTS{} bounds aggregate detection")
	}
	if ExprHasAgg(&ast.Func{Name: "size", Args: []ast.Expr{&ast.Var{Name: "a"}}}) {
		t.Fatal("scalar call is not an aggregate")
	}
	if !ExprHasAgg(&ast.Case{Whens: []ast.CaseWhen{{Cond: &ast.Lit{Value: ast.BoolLit(true)}, Result: agg}}}) {
		t.Fatal("CASE result arm")
	}
}

func mustBindErr(t *testing.T, err error, want string) {
	t.Helper()
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindBind {
		t.Fatalf("want KindBind error, got %v", err)
	}
	if !strings.Contains(serr.Msg, want) {
		t.Fatalf("error %q does not mention %q", serr.Msg, want)
	}
}

func TestCheckRefs(t *testing.T) {
	scope := map[string]int{"a": 0, "b": 1}
	ok := func(e ast.Expr) {
		t.Helper()
		if err := CheckRefs(e, scope); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	}
	ok(&ast.Prop{Var: "a", Key: "x"})
	ok(&ast.Binary{Op: ast.OpLt, LHS: &ast.Var{Name: "a"}, RHS: &ast.Var{Name: "b"}})
	ok(&ast.Lit{Value: ast.IntLit(1)})
	ok(&ast.HasLabelExpr{Var: "a", Expr: &ast.LabelExpr{Kind: ast.LabelName, Name: "P"}})

	mustBindErr(t, CheckRefs(&ast.Var{Name: "zz"}, scope), "unknown variable `zz`")
	mustBindErr(t, CheckRefs(&ast.Prop{Var: "zz", Key: "x"}, scope), "zz")
	mustBindErr(t, CheckRefs(&ast.Func{Name: "frobnicate", Args: []ast.Expr{}}, scope), "unknown function `frobnicate`")
	mustBindErr(t, CheckRefs(&ast.Func{Name: "frobnicate", Star: true}, scope), "unknown function")

	// ListPred: iteration var visible only in the predicate.
	ok(&ast.ListPred{Quant: ast.QuantAny, Var: "x",
		List: &ast.Prop{Var: "a", Key: "xs"},
		Pred: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "x"}, RHS: &ast.Var{Name: "b"}}})
	mustBindErr(t, CheckRefs(&ast.ListPred{Quant: ast.QuantAll, Var: "x",
		List: &ast.Var{Name: "x"},
		Pred: &ast.Lit{Value: ast.BoolLit(true)}}, scope), "unknown variable `x`")

	// Reduce: acc and var visible in the body only.
	ok(&ast.Reduce{Acc: "t", Init: &ast.Lit{Value: ast.IntLit(0)}, Var: "z",
		List: &ast.Prop{Var: "a", Key: "xs"},
		Body: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "t"}, RHS: &ast.Var{Name: "z"}}})

	// ListComp: var visible in filter and map, not the source list.
	ok(&ast.ListComp{Var: "x", List: &ast.Prop{Var: "a", Key: "xs"},
		Filter: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "x"}, RHS: &ast.Lit{Value: ast.IntLit(0)}},
		Map:    &ast.Var{Name: "x"}})

	// EXISTS: the inner WHERE sees outer variables plus the pattern's own.
	pat := &ast.Pattern{Start: ast.NodePat{Var: "a"},
		Hops: []ast.PatternHop{{Rel: ast.RelPat{Var: "r"}, Node: ast.NodePat{Var: "c"}}}}
	ok(&ast.Exists{Pattern: pat,
		Where: &ast.Binary{Op: ast.OpEq,
			LHS: &ast.Prop{Var: "c", Key: "x"}, RHS: &ast.Prop{Var: "b", Key: "x"}}})
	mustBindErr(t, CheckRefs(&ast.CountSub{Pattern: pat, Where: &ast.Var{Name: "nope"}}, scope), "nope")

	// PatternComp: pattern vars bound for filter and projection.
	ok(&ast.PatternComp{Pattern: pat, Where: &ast.Prop{Var: "r", Key: "w"}, Proj: &ast.Prop{Var: "c", Key: "n"}})

	// MapProj: the base variable must be bound; computed fields validate.
	ok(&ast.MapProj{Var: "a", Entries: []ast.MapProjEntry{
		{Kind: ast.MapProjProp, Key: "name"},
		{Kind: ast.MapProjField, Key: "c", Expr: &ast.Var{Name: "b"}}}})
	mustBindErr(t, CheckRefs(&ast.MapProj{Var: "zz"}, scope), "zz")

	// Cost references its two endpoints.
	ok(&ast.Cost{From: "a", To: "b"})
	mustBindErr(t, CheckRefs(&ast.Cost{From: "a", To: "zz"}, scope), "zz")

	// Case / In / IsNull / Index / Slice / PropOf / MapLit recurse.
	ok(&ast.Case{Operand: &ast.Var{Name: "a"},
		Whens: []ast.CaseWhen{{Cond: &ast.Lit{Value: ast.IntLit(1)}, Result: &ast.Var{Name: "b"}}},
		Else:  &ast.Var{Name: "a"}})
	ok(&ast.In{Expr: &ast.Prop{Var: "a", Key: "x"}, List: &ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}}})
	ok(&ast.IsNull{Expr: &ast.Var{Name: "a"}})
	ok(&ast.Index{Base: &ast.Prop{Var: "a", Key: "xs"}, Idx: &ast.Lit{Value: ast.IntLit(0)}})
	ok(&ast.Slice{Base: &ast.Prop{Var: "a", Key: "xs"}, From: &ast.Lit{Value: ast.IntLit(0)}, To: &ast.Lit{Value: ast.IntLit(2)}})
	ok(&ast.PropOf{Base: &ast.Func{Name: "coalesce", Args: []ast.Expr{&ast.Var{Name: "a"}}}, Key: "name"})
	mustBindErr(t, CheckRefs(&ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: &ast.Var{Name: "zz"}}}}, scope), "zz")
}

func TestCheckRefsSkippingAgg(t *testing.T) {
	scope := map[string]int{"a": 0}
	// count(*) binds nothing.
	if err := CheckRefsSkippingAgg(&ast.Func{Name: "count", Star: true}, scope); err != nil {
		t.Fatal(err)
	}
	// An aggregate's arguments are still validated.
	mustBindErr(t, CheckRefsSkippingAgg(
		&ast.Func{Name: "sum", Args: []ast.Expr{&ast.Prop{Var: "zz", Key: "x"}}}, scope), "zz")
	// A non-aggregate goes through the full check.
	mustBindErr(t, CheckRefsSkippingAgg(&ast.Var{Name: "zz"}, scope), "zz")
	if err := CheckRefsSkippingAgg(&ast.Func{Name: "sum", Args: []ast.Expr{&ast.Prop{Var: "a", Key: "x"}}}, scope); err != nil {
		t.Fatal(err)
	}
}

func TestExprHasAggRemainingArms(t *testing.T) {
	agg := &ast.Func{Name: "sum", Args: []ast.Expr{&ast.Prop{Var: "a", Key: "x"}}}
	lit := &ast.Lit{Value: ast.IntLit(1)}
	positives := []ast.Expr{
		&ast.Unary{Op: ast.Neg, Expr: agg},
		&ast.In{Expr: agg, List: &ast.ListExpr{}},
		&ast.In{Expr: lit, List: agg},
		&ast.IsNull{Expr: agg},
		&ast.ListExpr{Elems: []ast.Expr{lit, agg}},
		&ast.ListPred{Quant: ast.QuantAll, Var: "x", List: agg, Pred: lit},
		&ast.Reduce{Acc: "t", Init: lit, Var: "z", List: lit, Body: agg},
		&ast.ListComp{Var: "x", List: lit, Filter: agg},
		&ast.ListComp{Var: "x", List: lit, Map: agg},
		&ast.Index{Base: agg, Idx: lit},
		&ast.Slice{Base: lit, From: agg, To: nil},
		&ast.Slice{Base: lit, From: nil, To: agg},
		&ast.PropOf{Base: agg, Key: "k"},
		&ast.Case{Operand: agg, Whens: []ast.CaseWhen{{Cond: lit, Result: lit}}},
		&ast.Case{Whens: []ast.CaseWhen{{Cond: agg, Result: lit}}},
		&ast.Case{Whens: []ast.CaseWhen{{Cond: lit, Result: lit}}, Else: agg},
		&ast.Func{Name: "size", Args: []ast.Expr{agg}},
	}
	for i, e := range positives {
		if !ExprHasAgg(e) {
			t.Fatalf("positives[%d] must contain an aggregate", i)
		}
	}
	negatives := []ast.Expr{
		lit,
		&ast.Var{Name: "a"},
		&ast.Prop{Var: "a", Key: "x"},
		&ast.HasLabelExpr{Var: "a"},
		&ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: lit}}},
		&ast.MapProj{Var: "a", Entries: []ast.MapProjEntry{{Kind: ast.MapProjProp, Key: "n"}}},
		&ast.ListComp{Var: "x", List: lit},
		&ast.Slice{Base: lit},
	}
	for i, e := range negatives {
		if ExprHasAgg(e) {
			t.Fatalf("negatives[%d] must not contain an aggregate", i)
		}
	}
}

func TestErrorRendering(t *testing.T) {
	if bindErrf("unknown variable `%s`", "q").Error() != "unknown variable `q`" {
		t.Fatal("bind message")
	}
	if planErrf("nope").Kind != KindPlan {
		t.Fatal("plan kind")
	}
}
