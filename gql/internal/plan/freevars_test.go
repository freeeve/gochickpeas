package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/ast/asttest"
)

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

// TestFreeVarsReportsEveryKind pins the reference DETECTOR's error
// polarity (task 240): a rewriter that misses an arm leaves an expression
// untouched, but a detector that misses an arm under-reports a reference
// -- and a caller that trusts "unreferenced" then erases or captures a
// binding still in use (shadowFiltered, alphaLocal's fresh-name pool, the
// reorder correlation guard, weight validation all consume this walker).
// So the descent must report the reference from every Expr kind, pinned
// per kind with the shared roll call. TestRenameFree's before/after
// comparison cannot catch symmetric under-reporting -- a kind invisible
// to the walker yields empty free sets on both sides and passes.
func TestFreeVarsReportsEveryKind(t *testing.T) {
	want := map[string][]string{
		"Lit":  {},
		"Cost": {"n", "q"},
	}
	covered := map[string]bool{}
	for _, c := range asttest.KindCases("n") {
		covered[c.Kind] = true
		w, ok := want[c.Kind]
		if !ok {
			w = []string{"n"}
		}
		if free := freeVarsOutside(c.Build(), nil); !slices.Equal(free, w) {
			t.Errorf("%s: detector reports %v, want %v", c.Kind, free, w)
		}
	}
	asttest.RollCall(t, covered)
}
