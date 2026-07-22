package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// Direct coverage for the pure free-variable and conjunction helpers the
// planner tests reach only through full Build runs: the mention predicate,
// the variable collectors, and the AST AND combinators.

// TestMentionsVar covers the reference predicate, including its
// over-collecting descent into subquery patterns (binder locals count).
func TestMentionsVar(t *testing.T) {
	e := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "foo"}, RHS: &ast.Var{Name: "y"}}
	if !MentionsVar(e, "x") || !MentionsVar(e, "y") {
		t.Fatal("x and y are both mentioned")
	}
	if MentionsVar(e, "z") {
		t.Fatal("z is not mentioned")
	}
	if MentionsVar(nil, "x") {
		t.Fatal("nil expr mentions nothing")
	}
	// A subquery pattern's local variable is over-collected (never unsound).
	ex := &ast.Exists{
		Pattern: &ast.Pattern{Start: ast.NodePat{Var: "inner"}},
		Where:   &ast.Prop{Var: "inner", Key: "k"},
	}
	if !MentionsVar(ex, "inner") {
		t.Fatal("EXISTS pattern local should be over-collected")
	}
}

// TestExprVars covers the read-variable collector: bare refs and property
// bases, duplicates preserved.
func TestExprVars(t *testing.T) {
	e := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "foo"}, RHS: &ast.Var{Name: "y"}}
	got := exprVars(e)
	if !slices.Contains(got, "x") || !slices.Contains(got, "y") {
		t.Fatalf("exprVars = %v, want x and y", got)
	}
	// Duplicates are kept (callers dedup).
	dup := &ast.Binary{Op: ast.OpEq, LHS: &ast.Var{Name: "x"}, RHS: &ast.Var{Name: "x"}}
	if got := exprVars(dup); len(got) != 2 {
		t.Fatalf("exprVars kept-duplicates = %v, want two", got)
	}
	if got := exprVars(&ast.Lit{Value: ast.IntLit(1)}); len(got) != 0 {
		t.Fatalf("literal reads no vars: %v", got)
	}
}

// TestPatternVars covers the pattern-variable lister: appearance order,
// anonymous slots skipped.
func TestPatternVars(t *testing.T) {
	p := &ast.Pattern{
		Start: ast.NodePat{Var: "a"},
		Hops: []ast.PatternHop{
			{Rel: ast.RelPat{Var: "r"}, Node: ast.NodePat{Var: "b"}},
			{Rel: ast.RelPat{Var: ""}, Node: ast.NodePat{Var: "c"}}, // anon rel skipped
		},
	}
	if got := patternVars(p); !slices.Equal(got, []string{"a", "r", "b", "c"}) {
		t.Fatalf("patternVars = %v, want [a r b c]", got)
	}
	// A fully anonymous pattern lists nothing.
	if got := patternVars(&ast.Pattern{Start: ast.NodePat{Var: ""}}); len(got) != 0 {
		t.Fatalf("anonymous pattern vars = %v", got)
	}
}

// TestAndWith covers conjoining onto a possibly-nil base.
func TestAndWith(t *testing.T) {
	extra := &ast.Var{Name: "e"}
	if got := andWith(nil, extra); got != ast.Expr(extra) {
		t.Fatal("nil base should return extra unchanged")
	}
	base := &ast.Var{Name: "b"}
	got, ok := andWith(base, extra).(*ast.Binary)
	if !ok || got.Op != ast.OpAnd || got.LHS != ast.Expr(base) || got.RHS != ast.Expr(extra) {
		t.Fatalf("andWith(base, extra) = %#v", got)
	}
}

// TestAndJoin covers folding a conjunct list into a left-leaning AND tree.
func TestAndJoin(t *testing.T) {
	if andJoin(nil) != nil {
		t.Fatal("empty conjuncts fold to nil")
	}
	a := &ast.Var{Name: "a"}
	if got := andJoin([]ast.Expr{a}); got != ast.Expr(a) {
		t.Fatal("single conjunct folds to itself")
	}
	b, c := &ast.Var{Name: "b"}, &ast.Var{Name: "c"}
	// Left-folded: ((a AND b) AND c).
	top, ok := andJoin([]ast.Expr{a, b, c}).(*ast.Binary)
	if !ok || top.Op != ast.OpAnd || top.RHS != ast.Expr(c) {
		t.Fatalf("andJoin top = %#v", top)
	}
	inner, ok := top.LHS.(*ast.Binary)
	if !ok || inner.LHS != ast.Expr(a) || inner.RHS != ast.Expr(b) {
		t.Fatalf("andJoin inner = %#v", inner)
	}
}

// TestConjSlotRefs covers resolving a conjunct's referenced segment slots,
// sorted, with names absent from the slot map ignored.
func TestConjSlotRefs(t *testing.T) {
	slots := map[string]int{"x": 5, "y": 2}
	e := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "k"}, RHS: &ast.Var{Name: "y"}}
	if got := conjSlotRefs(e, slots); !slices.Equal(got, []int{2, 5}) {
		t.Fatalf("conjSlotRefs = %v, want [2 5]", got)
	}
	// A variable not in the slot map (e.g. a subquery local) is ignored.
	if got := conjSlotRefs(&ast.Var{Name: "unknown"}, slots); len(got) != 0 {
		t.Fatalf("unknown var slots = %v", got)
	}
}
