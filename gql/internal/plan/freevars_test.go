package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
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
