package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/ast/asttest"
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

	// A label test on a variable key projected under a different name
	// repoints its variable at the output column, exactly like a property
	// access -- the arm the Rust walker missed (task 230).
	lbl := &ast.HasLabelExpr{Var: "n", Expr: &ast.LabelExpr{Name: "Company"}}
	hl, ok := substGroupKeys(lbl, renamed).(*ast.HasLabelExpr)
	if !ok || hl.Var != "m" || hl.Expr != lbl.Expr {
		t.Fatalf("label-test repoint = %#v, want HasLabelExpr(m:Company)", substGroupKeys(lbl, renamed))
	}
	// Nested inside a CASE arm it is rewritten in place.
	cs := &ast.Case{Whens: []ast.CaseWhen{{
		Cond:   &ast.HasLabelExpr{Var: "n", Expr: &ast.LabelExpr{Name: "Company"}},
		Result: &ast.Lit{Value: ast.IntLit(1)},
	}}}
	out, ok := substGroupKeys(cs, renamed).(*ast.Case)
	if !ok {
		t.Fatalf("case rewrite = %#v", substGroupKeys(cs, renamed))
	}
	if w, ok := out.Whens[0].Cond.(*ast.HasLabelExpr); !ok || w.Var != "m" {
		t.Fatalf("label test inside CASE = %#v, want Var m", out.Whens[0].Cond)
	}
	// A same-name key leaves the label test untouched.
	if hl2, ok := substGroupKeys(&ast.HasLabelExpr{Var: "n", Expr: lbl.Expr}, same).(*ast.HasLabelExpr); !ok || hl2.Var != "n" {
		t.Fatal("a same-name key must leave the label test on its variable")
	}
}

// substKindCase adapts the shared asttest kind table to this package's
// walker policies: build returns a fresh instance referencing `n`, want
// states what a rename of n to the output column m must leave free.
type substKindCase struct {
	kind  string
	build func() ast.Expr
	want  []string
}

// substKindCases returns the asttest per-kind table with substGroupKeys'
// carve-outs attached. Absent from the map means every reference follows
// the key: sources and binder bodies both rewrite (locals only filter the
// groups they shadow, see TestSubstGroupKeysShadowing). Correlated
// subquery filters keep the bare name -- they evaluate against the
// subquery's own pattern scope -- and Cost is a pattern-clause construct
// a projection wrapper cannot hold.
func substKindCases() []substKindCase {
	want := map[string][]string{
		"Exists":      {"n"},
		"CountSub":    {"n"},
		"PatternComp": {"n"},
		"Cost":        {"n", "q"},
	}
	var out []substKindCase
	for _, c := range asttest.KindCases("n") {
		out = append(out, substKindCase{kind: c.Kind, build: c.Build, want: want[c.Kind]})
	}
	return out
}

// requireKindRollCall delegates to the shared roll call so every walker
// test in this package stays pinned to the full ast.Expr kind set.
func requireKindRollCall(t *testing.T, covered map[string]bool) {
	t.Helper()
	asttest.RollCall(t, covered)
}

// TestSubstGroupKeysShadowing pins the binder-scope policy of the descent
// (tasks 232/233): a body reference the binder shadows stays the local, a
// body reference to an unshadowed key rewrites, and a binder local
// colliding with a key's output name drops that key for the sub-scope --
// the outer reference then stays free (a clean bind error downstream)
// instead of being captured; alpha-renaming the local would lift this.
func TestSubstGroupKeysShadowing(t *testing.T) {
	groups := []groupCol{{idx: 0, name: "m", expr: &ast.Var{Name: "n"}}}
	one := &ast.Lit{Value: ast.IntLit(1)}

	// [x IN [1] | x + n.val]: the body's n is the renamed key -- rewrites.
	lc := &ast.ListComp{Var: "x", List: &ast.ListExpr{Elems: []ast.Expr{one}},
		Map: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "x"}, RHS: &ast.Prop{Var: "n", Key: "val"}}}
	got := substGroupKeys(lc, groups).(*ast.ListComp)
	if free := freeVarsOutside(got, []string{"m"}); len(free) != 0 {
		t.Fatalf("unshadowed body ref: free = %v, want none", free)
	}

	// [n IN [1] | n]: the local shadows the key's source variable -- the
	// body's n means the local and must NOT be substituted.
	shadow := &ast.ListComp{Var: "n", List: &ast.ListExpr{Elems: []ast.Expr{one}},
		Map: &ast.Var{Name: "n"}}
	got = substGroupKeys(shadow, groups).(*ast.ListComp)
	if v, ok := got.Map.(*ast.Var); !ok || v.Name != "n" {
		t.Fatalf("shadowed body ref = %#v, want the untouched local n", got.Map)
	}

	// [m IN [1] | m + n.x]: the local collides with the key's OUTPUT name.
	// The local is alpha-renamed to a fresh name, the body's local refs
	// follow it, and the outer n then substitutes to the column -- no
	// capture, no residual free reference (task 233).
	collide := &ast.ListComp{Var: "m", List: &ast.ListExpr{Elems: []ast.Expr{one}},
		Map: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "m"}, RHS: &ast.Prop{Var: "n", Key: "x"}}}
	got = substGroupKeys(collide, groups).(*ast.ListComp)
	if got.Var == "m" {
		t.Fatalf("colliding local was not alpha-renamed: %#v", got)
	}
	bin := got.Map.(*ast.Binary)
	if v, ok := bin.LHS.(*ast.Var); !ok || v.Name != got.Var {
		t.Fatalf("local body ref = %#v, want the renamed local %q", bin.LHS, got.Var)
	}
	po, ok := bin.RHS.(*ast.PropOf)
	if !ok || asVarName(po.Base) != "m" {
		t.Fatalf("outer key ref = %#v, want PropOf(m).x", bin.RHS)
	}
	if free := freeVarsOutside(got, []string{"m"}); len(free) != 0 {
		t.Fatalf("colliding local: free = %v, want none after alpha-rename", free)
	}
}

// TestRenameFree asserts the alpha-rename over every ast.Expr kind: each
// free reference to the source name moves to the target, shadowing binders
// stop the descent, and nothing else changes.
func TestRenameFree(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range substKindCases() {
		covered[c.kind] = true
		before := freeVarsOutside(c.build(), nil)
		got := renameFree(c.build(), "n", "n2")
		want := make([]string, 0, len(before))
		for _, v := range before {
			if v == "n" {
				v = "n2"
			}
			want = append(want, v)
		}
		slices.Sort(want)
		if free := freeVarsOutside(got, nil); !slices.Equal(free, want) {
			t.Errorf("%s: free vars after rename = %v, want %v", c.kind, free, want)
		}
	}
	requireKindRollCall(t, covered)

	// A binder that rebinds the source name stops the descent: the body's
	// references mean the local and must stay.
	one := &ast.Lit{Value: ast.IntLit(1)}
	shadow := &ast.ListComp{Var: "n", List: &ast.ListExpr{Elems: []ast.Expr{one}},
		Map: &ast.Var{Name: "n"}}
	got := renameFree(shadow, "n", "n2").(*ast.ListComp)
	if v, ok := got.Map.(*ast.Var); !ok || v.Name != "n" || got.Var != "n" {
		t.Fatalf("shadowing binder descended: %#v", got)
	}
}

// TestSubstGroupKeysInvariant asserts the rename invariant over every
// ast.Expr kind (task 231): after substituting a key projected under a
// different name, no expression references a variable no surviving binder
// provides, except the pinned policy carve-outs in substKindCases.
func TestSubstGroupKeysInvariant(t *testing.T) {
	groups := []groupCol{{idx: 0, name: "m", expr: &ast.Var{Name: "n"}}}
	covered := map[string]bool{}
	for _, c := range substKindCases() {
		covered[c.kind] = true
		got := substGroupKeys(c.build(), groups)
		free := freeVarsOutside(got, []string{"m"})
		want := c.want
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(free, want) {
			t.Errorf("%s: free vars after rename = %v, want %v", c.kind, free, want)
		}
	}
	requireKindRollCall(t, covered)
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
