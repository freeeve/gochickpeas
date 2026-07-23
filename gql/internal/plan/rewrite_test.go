package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
)

// TestSubstExprPreservesPostfixOps pins that alias inlining preserves the
// postfix predicate kind. A prior copy-paste rebuilt IS TRUE and IS TYPED as
// IS NULL when their operand was an inlined alias, silently changing the
// predicate (and dropping IsTruth.Want / IsTyped.Kind).
func TestSubstExprPreservesPostfixOps(t *testing.T) {
	subst := map[string]ast.Expr{"m": &ast.Var{Name: "n"}}

	// IS [NOT] TRUE/FALSE keeps its kind, its Want, and its Negated flag.
	got, ok := substExpr(&ast.IsTruth{Expr: &ast.Var{Name: "m"}, Want: false, Negated: true}, subst)
	if !ok {
		t.Fatal("IsTruth substitution declined")
	}
	tr, isTruth := got.(*ast.IsTruth)
	if !isTruth {
		t.Fatalf("IS TRUE rewrote to %T, want *ast.IsTruth", got)
	}
	if tr.Want != false || tr.Negated != true {
		t.Fatalf("IsTruth fields lost: %+v", tr)
	}
	if v, isVar := tr.Expr.(*ast.Var); !isVar || v.Name != "n" {
		t.Fatalf("IsTruth operand = %+v, want Var(n)", tr.Expr)
	}

	// IS [NOT] TYPED carries its Kind through.
	got, ok = substExpr(&ast.IsTyped{Expr: &ast.Var{Name: "m"}, Kind: "integer", Negated: true}, subst)
	if !ok {
		t.Fatal("IsTyped substitution declined")
	}
	ty, isTyped := got.(*ast.IsTyped)
	if !isTyped {
		t.Fatalf("IS TYPED rewrote to %T, want *ast.IsTyped", got)
	}
	if ty.Kind != "integer" || ty.Negated != true {
		t.Fatalf("IsTyped fields lost: %+v", ty)
	}

	// IS NULL is the baseline that must stay IS NULL.
	got, _ = substExpr(&ast.IsNull{Expr: &ast.Var{Name: "m"}, Negated: true}, subst)
	if _, isNull := got.(*ast.IsNull); !isNull {
		t.Fatalf("IS NULL rewrote to %T, want *ast.IsNull", got)
	}

	// A property test on an inlined alias rewrites the property's variable and
	// still returns an IS TRUE wrapping the rewritten property.
	got, ok = substExpr(&ast.IsTruth{Expr: &ast.Prop{Var: "m", Key: "k"}, Want: true}, subst)
	if !ok {
		t.Fatal("nested IsTruth substitution declined")
	}
	tr, isTruth = got.(*ast.IsTruth)
	if !isTruth {
		t.Fatalf("nested predicate = %T, want *ast.IsTruth", got)
	}
	if p, isProp := tr.Expr.(*ast.Prop); !isProp || p.Var != "n" || p.Key != "k" {
		t.Fatalf("rewritten operand = %+v, want Prop(n.k)", tr.Expr)
	}
}

// TestFuseProjectionKeepsPostfixPredicate drives the whole projection-fusion
// pass: a pure WITH folded into a following aggregating WITH must inline the
// alias into an IS TRUE predicate without turning it into IS NULL.
func TestFuseProjectionKeepsPostfixPredicate(t *testing.T) {
	q, err := parser.Parse("MATCH (n:N) RETURN n.flag AS f NEXT RETURN f IS TRUE AS t, count(*) AS c NEXT RETURN t, c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	clauses := q.Parts[0].Clauses
	fused := fuseProjectionBeforeAggregate(clauses)
	// The pure projection collapses into the aggregate, so one clause leaves.
	if len(fused) != len(clauses)-1 {
		t.Fatalf("fusion did not fire: %d clauses -> %d", len(clauses), len(fused))
	}

	var agg *ast.With
	for _, c := range fused {
		if w, isWith := c.(*ast.With); isWith && projectionIsAggregated(&w.Proj) {
			agg = w
		}
	}
	if agg == nil {
		t.Fatal("no aggregating With survived the fusion")
	}
	var tItem *ast.ReturnItem
	for i := range agg.Proj.Items {
		if agg.Proj.Items[i].Alias == "t" {
			tItem = &agg.Proj.Items[i]
		}
	}
	if tItem == nil {
		t.Fatal("fused projection dropped the 't' column")
	}
	tr, ok := tItem.Expr.(*ast.IsTruth)
	if !ok {
		t.Fatalf("fused predicate = %T, want *ast.IsTruth (bug rebuilt it as IsNull)", tItem.Expr)
	}
	// The inlined operand proves the substitution actually ran (f -> n.flag).
	if p, isProp := tr.Expr.(*ast.Prop); !isProp || p.Var != "n" || p.Key != "flag" {
		t.Fatalf("inlined operand = %+v, want Prop(n.flag)", tr.Expr)
	}
}
