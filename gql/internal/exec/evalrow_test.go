package exec

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// TestAstPushdown covers the interpreted path's conservative slot analysis:
// bound variable/property/label reads collect their row slots, the boolean
// and list/case forms recurse into their children, an unbound name collects
// nothing, and any subquery/function/comprehension form pins the expression
// to the last op level (hasSlow).
func TestAstPushdown(t *testing.T) {
	slots := map[string]int{"x": 0, "y": 3, "z": 7}
	run := func(e ast.Expr) ([]int, bool) {
		var refs []int
		var slow bool
		astPushdown(e, slots, &refs, &slow)
		return refs, slow
	}

	// A literal reads no slots and is not slow.
	if refs, slow := run(&ast.Lit{Value: ast.IntLit(1)}); len(refs) != 0 || slow {
		t.Fatalf("literal = %v,%v", refs, slow)
	}
	// A bound variable reads its slot; an unbound name reads nothing.
	if refs, slow := run(&ast.Var{Name: "x"}); !slices.Equal(refs, []int{0}) || slow {
		t.Fatalf("var x = %v,%v", refs, slow)
	}
	if refs, _ := run(&ast.Var{Name: "unbound"}); len(refs) != 0 {
		t.Fatalf("unbound var = %v", refs)
	}
	// Property base and boolean binary collect both operands.
	bin := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "k"}, RHS: &ast.Var{Name: "y"}}
	if refs, slow := run(bin); !slices.Equal(refs, []int{0, 3}) || slow {
		t.Fatalf("binary = %v,%v", refs, slow)
	}
	// A label test collects its variable.
	if refs, _ := run(&ast.HasLabelExpr{Var: "y"}); !slices.Equal(refs, []int{3}) {
		t.Fatalf("hasLabel = %v", refs)
	}
	// IS NULL over a property collects the base.
	if refs, _ := run(&ast.IsNull{Expr: &ast.Prop{Var: "z", Key: "k"}}); !slices.Equal(refs, []int{7}) {
		t.Fatalf("isNull = %v", refs)
	}
	// IN over a list collects the probe and every list element.
	in := &ast.In{Expr: &ast.Var{Name: "x"}, List: &ast.ListExpr{Elems: []ast.Expr{&ast.Var{Name: "z"}, &ast.Lit{Value: ast.IntLit(1)}}}}
	if refs, slow := run(in); !slices.Equal(refs, []int{0, 7}) || slow {
		t.Fatalf("IN = %v,%v", refs, slow)
	}
	// CASE collects operand, when-cond, result, and skips the literal else.
	cs := &ast.Case{
		Operand: &ast.Var{Name: "x"},
		Whens:   []ast.CaseWhen{{Cond: &ast.Var{Name: "y"}, Result: &ast.Var{Name: "z"}}},
		Else:    &ast.Lit{Value: ast.IntLit(0)},
	}
	if refs, slow := run(cs); !slices.Equal(refs, []int{0, 3, 7}) || slow {
		t.Fatalf("CASE = %v,%v", refs, slow)
	}
	// Unary, IS TRUE, and IS TYPED all recurse into their single operand.
	if refs, _ := run(&ast.Unary{Op: ast.Neg, Expr: &ast.Var{Name: "x"}}); !slices.Equal(refs, []int{0}) {
		t.Fatalf("unary = %v", refs)
	}
	if refs, _ := run(&ast.IsTruth{Expr: &ast.Var{Name: "y"}}); !slices.Equal(refs, []int{3}) {
		t.Fatalf("isTruth = %v", refs)
	}
	if refs, _ := run(&ast.IsTyped{Expr: &ast.Var{Name: "z"}}); !slices.Equal(refs, []int{7}) {
		t.Fatalf("isTyped = %v", refs)
	}
	// Subquery and function forms pin to the last level.
	if _, slow := run(&ast.Exists{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "n"}}}); !slow {
		t.Fatal("EXISTS must set hasSlow")
	}
	if _, slow := run(&ast.Func{Name: "abs", Args: []ast.Expr{&ast.Var{Name: "x"}}}); !slow {
		t.Fatal("a function call must set hasSlow")
	}
}

// TestInterpExprEval covers the interpreter-fallback RowEval: interpExpr.Eval
// runs the AST through the interpreter, which exec's own native-graph tests
// otherwise route around via the compiled form.
func TestInterpExprEval(t *testing.T) {
	g := graph.New(chickpeas.NewBuilder(1, 0).Finalize())
	ctx := &eval.Ctx{G: g}

	// A constant arithmetic expression evaluates through the interpreter.
	sum := interpExpr{e: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Lit{Value: ast.IntLit(2)}, RHS: &ast.Lit{Value: ast.IntLit(3)}}}
	if v, _ := sum.Eval(ctx, nil, nil).AsInt(); v != 5 {
		t.Fatalf("interpExpr.Eval(2 + 3) = %v, want 5", sum.Eval(ctx, nil, nil))
	}
	// A row slot read resolves against the passed row and slot map.
	slotRead := interpExpr{e: &ast.Var{Name: "x"}}
	if v, _ := slotRead.Eval(ctx, urow(7), map[string]int{"x": 0}).AsInt(); v != 7 {
		t.Fatalf("interpExpr.Eval(x) over row[0]=7 = %v, want 7", slotRead.Eval(ctx, urow(7), map[string]int{"x": 0}))
	}
}
