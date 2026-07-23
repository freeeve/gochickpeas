package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestSubstGroupKeys covers the grouping-key substitution: a subexpression
// structurally equal to a grouping item becomes a reference to its output
// column (directly and nested inside a wrapper), a property on a variable key
// projected under a different name repoints to a PropOf on that column, and a
// same-name key or an unrelated subexpression is left untouched.
func TestSubstGroupKeys(t *testing.T) {
	add := func(l, r ast.Expr) ast.Expr { return &ast.Binary{Op: ast.OpAdd, LHS: l, RHS: r} }
	v := func(n string) ast.Expr { return &ast.Var{Name: n} }

	// A wrapper subexpression equal to a grouping key becomes Var(column).
	groups := []groupCol{{idx: 0, name: "g", expr: add(v("a"), v("b"))}}
	if got := substGroupKeys(add(v("a"), v("b")), groups); asVarName(got) != "g" {
		t.Fatalf("matching key = %#v, want Var(g)", got)
	}
	// The same match nested inside another expression is rewritten in place.
	outer := &ast.Binary{Op: ast.OpMul, LHS: add(v("a"), v("b")), RHS: &ast.Lit{Value: ast.IntLit(2)}}
	got, ok := substGroupKeys(outer, groups).(*ast.Binary)
	if !ok || asVarName(got.LHS) != "g" {
		t.Fatalf("nested match = %#v", substGroupKeys(outer, groups))
	}

	// A property on a variable key projected under a different name repoints to
	// PropOf(column).key.
	renamed := []groupCol{{idx: 0, name: "m", expr: v("n")}}
	po, ok := substGroupKeys(&ast.Prop{Var: "n", Key: "x"}, renamed).(*ast.PropOf)
	if !ok || po.Key != "x" || asVarName(po.Base) != "m" {
		t.Fatalf("prop repoint = %#v, want PropOf(m.x)", substGroupKeys(&ast.Prop{Var: "n", Key: "x"}, renamed))
	}
	// A key projected under its own name leaves the property as a plain Prop.
	same := []groupCol{{idx: 0, name: "n", expr: v("n")}}
	if _, ok := substGroupKeys(&ast.Prop{Var: "n", Key: "x"}, same).(*ast.Prop); !ok {
		t.Fatal("a same-name key must leave the property as a Prop")
	}
	// An unrelated subexpression is unchanged.
	if _, ok := substGroupKeys(&ast.Lit{Value: ast.IntLit(5)}, groups).(*ast.Lit); !ok {
		t.Fatal("an unrelated literal must be unchanged")
	}
}

// TestExtractAgg covers the top-level aggregate compiler: each supported
// function name maps to its AggKind, count(*) and DISTINCT are handled, the
// percentile aggregates demand exactly two arguments with a constant second,
// and the invalid shapes error.
func TestExtractAgg(t *testing.T) {
	v := &ast.Var{Name: "x"}
	kinds := map[string]AggKind{
		"count": AggCount, "sum": AggSum, "avg": AggAvg, "min": AggMin, "max": AggMax,
		"collect": AggCollect, "collect_list": AggCollect,
		"stddev_samp": AggStddevSamp, "stddev_pop": AggStddevPop,
	}
	for name, want := range kinds {
		ac, err := extractAgg(&ast.Func{Name: name, Args: []ast.Expr{v}}, 3)
		if err != nil || ac.Kind != want || ac.OutIdx != 3 || ac.Arg != ast.Expr(v) {
			t.Fatalf("%s -> %+v, err=%v", name, ac, err)
		}
	}

	// count(*) is valid, and DISTINCT carries through.
	if ac, err := extractAgg(&ast.Func{Name: "count", Star: true}, 0); err != nil || ac.Kind != AggCount || ac.Arg != nil {
		t.Fatalf("count(*) = %+v, err=%v", ac, err)
	}
	if ac, err := extractAgg(&ast.Func{Name: "count", Distinct: true, Args: []ast.Expr{v}}, 0); err != nil || !ac.Distinct {
		t.Fatalf("count(DISTINCT x) = %+v, err=%v", ac, err)
	}

	// A percentile aggregate keeps both arguments when the second is constant.
	pct := &ast.Func{Name: "percentile_cont", Args: []ast.Expr{v, &ast.Lit{Value: ast.FloatLit(0.5)}}}
	if ac, err := extractAgg(pct, 1); err != nil || ac.Kind != AggPercentileCont || ac.Arg2 == nil {
		t.Fatalf("percentile_cont = %+v, err=%v", ac, err)
	}

	// The error shapes.
	bad := map[string]ast.Expr{
		"not a func":           &ast.Var{Name: "x"},
		"unknown agg":          &ast.Func{Name: "median", Args: []ast.Expr{v}},
		"too many args":        &ast.Func{Name: "sum", Args: []ast.Expr{v, v}},
		"sum(*) invalid":       &ast.Func{Name: "sum", Star: true},
		"percentile one arg":   &ast.Func{Name: "percentile_cont", Args: []ast.Expr{v}},
		"percentile non-const": &ast.Func{Name: "percentile_cont", Args: []ast.Expr{v, v}},
	}
	for name, e := range bad {
		if _, err := extractAgg(e, 0); err == nil {
			t.Fatalf("%s should error", name)
		}
	}
}
