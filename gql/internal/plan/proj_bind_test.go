package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestExtractNestedAggsPreservesPostfixOps pins that hoisting a nested
// aggregate out of a postfix predicate keeps the predicate's kind. A prior
// copy-paste rebuilt IS TRUE and IS TYPED as IS NULL, silently changing the
// predicate (and dropping IsTruth.Want / IsTyped.Kind) whenever an aggregate
// appeared inside one -- e.g. `RETURN sum(x) IS TYPED INTEGER`.
func TestExtractNestedAggsPreservesPostfixOps(t *testing.T) {
	agg := func(name string) ast.Expr {
		return &ast.Func{Name: name, Args: []ast.Expr{&ast.Var{Name: "x"}}}
	}
	run := func(e ast.Expr) (ast.Expr, []AggCol) {
		t.Helper()
		var hidden int
		var aggs []AggCol
		out, err := extractNestedAggs(e, 2, &hidden, &aggs)
		if err != nil {
			t.Fatalf("extractNestedAggs(%#v): %v", e, err)
		}
		return out, aggs
	}

	// sum(x) IS NOT TYPED INTEGER: the aggregate is hoisted and the wrapper
	// stays an IS TYPED carrying its Kind and Negated flag.
	out, aggs := run(&ast.IsTyped{Expr: agg("sum"), Kind: "integer", Negated: true})
	ty, ok := out.(*ast.IsTyped)
	if !ok {
		t.Fatalf("IS TYPED wrapper became %T, want *ast.IsTyped", out)
	}
	if ty.Kind != "integer" || !ty.Negated {
		t.Fatalf("IsTyped fields lost: %+v", ty)
	}
	if len(aggs) != 1 || aggs[0].Kind != AggSum {
		t.Fatalf("aggregate not hoisted: %+v", aggs)
	}
	if _, isVar := ty.Expr.(*ast.Var); !isVar {
		t.Fatalf("inner operand = %#v, want a hoisted aggregate Var", ty.Expr)
	}

	// count(x) IS NOT TRUE keeps its Want and stays an IS TRUE.
	out, _ = run(&ast.IsTruth{Expr: agg("count"), Want: false, Negated: true})
	tr, ok := out.(*ast.IsTruth)
	if !ok {
		t.Fatalf("IS TRUE wrapper became %T, want *ast.IsTruth", out)
	}
	if tr.Want != false || !tr.Negated {
		t.Fatalf("IsTruth fields lost: %+v", tr)
	}

	// IS NULL over an aggregate is the baseline that must stay IS NULL.
	out, _ = run(&ast.IsNull{Expr: agg("count"), Negated: true})
	if _, ok := out.(*ast.IsNull); !ok {
		t.Fatalf("IS NULL wrapper became %T, want *ast.IsNull", out)
	}
}
