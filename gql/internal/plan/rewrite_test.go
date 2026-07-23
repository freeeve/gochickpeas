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

// TestSubstExprCompoundArms exercises the recursive arms: every compound node
// rewrites its children and keeps its own kind, a child that declines aborts
// the whole node, and a construct the pass does not rewrite (a subquery)
// declines outright.
func TestSubstExprCompoundArms(t *testing.T) {
	toN := map[string]ast.Expr{"m": &ast.Var{Name: "n"}}
	// isN reports whether e is the substituted Var(n).
	isN := func(e ast.Expr) bool {
		v, ok := e.(*ast.Var)
		return ok && v.Name == "n"
	}
	m := func() ast.Expr { return &ast.Var{Name: "m"} }

	// Each compound node recurses into its children and preserves its kind.
	unary, ok := substExpr(&ast.Unary{Op: ast.UnOp(0), Expr: m()}, toN)
	if u, isU := unary.(*ast.Unary); !ok || !isU || !isN(u.Expr) {
		t.Fatalf("Unary arm = %+v (ok=%v)", unary, ok)
	}
	bin, ok := substExpr(&ast.Binary{Op: ast.BinOp(0), LHS: m(), RHS: m()}, toN)
	if b, isB := bin.(*ast.Binary); !ok || !isB || !isN(b.LHS) || !isN(b.RHS) {
		t.Fatalf("Binary arm = %+v (ok=%v)", bin, ok)
	}
	in, ok := substExpr(&ast.In{Expr: m(), List: &ast.ListExpr{Elems: []ast.Expr{m()}}}, toN)
	if i, isI := in.(*ast.In); !ok || !isI || !isN(i.Expr) {
		t.Fatalf("In arm = %+v (ok=%v)", in, ok)
	}
	fn, ok := substExpr(&ast.Func{Name: "abs", Args: []ast.Expr{m()}}, toN)
	if f, isF := fn.(*ast.Func); !ok || !isF || f.Name != "abs" || !isN(f.Args[0]) {
		t.Fatalf("Func arm = %+v (ok=%v)", fn, ok)
	}
	list, ok := substExpr(&ast.ListExpr{Elems: []ast.Expr{m(), &ast.Lit{}}}, toN)
	if l, isL := list.(*ast.ListExpr); !ok || !isL || !isN(l.Elems[0]) {
		t.Fatalf("ListExpr arm = %+v (ok=%v)", list, ok)
	}
	cs, ok := substExpr(&ast.Case{
		Operand: m(),
		Whens:   []ast.CaseWhen{{Cond: m(), Result: m()}},
		Else:    m(),
	}, toN)
	if c, isC := cs.(*ast.Case); !ok || !isC ||
		!isN(c.Operand) || !isN(c.Whens[0].Cond) || !isN(c.Whens[0].Result) || !isN(c.Else) {
		t.Fatalf("Case arm = %+v (ok=%v)", cs, ok)
	}

	// count(*) has no argument to rewrite and passes through unchanged.
	star := &ast.Func{Name: "count", Star: true}
	if got, ok := substExpr(star, toN); !ok || got != ast.Expr(star) {
		t.Fatalf("star Func should pass through unchanged, got %+v (ok=%v)", got, ok)
	}
	// A literal is returned as-is.
	litIn := &ast.Lit{}
	if got, ok := substExpr(litIn, toN); !ok || got != ast.Expr(litIn) {
		t.Fatal("Lit should pass through unchanged")
	}

	// HasLabelExpr rewrites when the alias maps to a bare variable...
	if got, ok := substExpr(&ast.HasLabelExpr{Var: "m"}, toN); !ok {
		t.Fatalf("HasLabelExpr over a var alias should rewrite, ok=%v", ok)
	} else if h, isH := got.(*ast.HasLabelExpr); !isH || h.Var != "n" {
		t.Fatalf("HasLabelExpr arm = %+v", got)
	}

	// ...but a property or label test on an alias mapped to a NON-variable
	// (here a property) cannot be rewritten, so the whole node declines --
	// and that decline propagates out through any enclosing compound.
	toProp := map[string]ast.Expr{"m": &ast.Prop{Var: "x", Key: "y"}}
	if _, ok := substExpr(&ast.Prop{Var: "m", Key: "k"}, toProp); ok {
		t.Fatal("Prop on a non-var alias must decline")
	}
	if _, ok := substExpr(&ast.HasLabelExpr{Var: "m"}, toProp); ok {
		t.Fatal("HasLabelExpr on a non-var alias must decline")
	}
	if _, ok := substExpr(&ast.Binary{LHS: &ast.Prop{Var: "m", Key: "k"}, RHS: &ast.Lit{}}, toProp); ok {
		t.Fatal("a declining child must abort the enclosing Binary")
	}

	// A subquery is a scoped construct the pass never rewrites.
	if _, ok := substExpr(&ast.Exists{}, toN); ok {
		t.Fatal("Exists (a scoped subquery) must decline")
	}
}

// TestInlineProjection covers the aggregating-boundary rewrite: it inlines the
// substitution into every projection item (naming an unaliased item by its
// derived name), its ORDER BY keys, and its WHERE, and it declines whenever
// any of those contains a construct the substitution cannot rewrite.
func TestInlineProjection(t *testing.T) {
	// The alias `a` was a pure projection of `n.x`.
	subst := map[string]ast.Expr{"a": &ast.Prop{Var: "n", Key: "x"}}

	// A projection whose items, ORDER BY key, and WHERE all reference the
	// inlined alias substitutes cleanly into a With.
	proj := &ast.Projection{
		Items: []ast.ReturnItem{
			{Expr: &ast.Func{Name: "count", Star: true}, Alias: "c"},
			{Expr: &ast.Var{Name: "a"}}, // unaliased: named by DerivedName
		},
		OrderBy: []ast.SortItem{{Expr: &ast.Var{Name: "a"}, Desc: true}},
	}
	where := &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "a"}, RHS: &ast.Lit{Value: ast.IntLit(0)}}
	w, ok := inlineProjection(proj, where, subst)
	if !ok {
		t.Fatal("a fully substitutable projection must inline")
	}
	// The unaliased item keeps its derived name and is rewritten to n.x.
	if w.Proj.Items[1].Alias != "a" {
		t.Fatalf("unaliased item name = %q, want the derived name a", w.Proj.Items[1].Alias)
	}
	if _, isProp := w.Proj.Items[1].Expr.(*ast.Prop); !isProp {
		t.Fatalf("item not substituted: %#v", w.Proj.Items[1].Expr)
	}
	// The ORDER BY key and WHERE were substituted too.
	if _, isProp := w.Proj.OrderBy[0].Expr.(*ast.Prop); !isProp {
		t.Fatalf("order key not substituted: %#v", w.Proj.OrderBy[0].Expr)
	}
	if wb, _ := w.Where.(*ast.Binary); wb == nil {
		t.Fatal("WHERE not preserved")
	} else if _, isProp := wb.LHS.(*ast.Prop); !isProp {
		t.Fatalf("WHERE operand not substituted: %#v", wb.LHS)
	}

	// A projection item, an ORDER BY key, or a WHERE the substitution cannot
	// rewrite (a scoped subquery) aborts the inline.
	item := func(e ast.Expr) []ast.ReturnItem { return []ast.ReturnItem{{Expr: e, Alias: "x"}} }
	if _, ok := inlineProjection(&ast.Projection{Items: item(&ast.Exists{})}, nil, subst); ok {
		t.Fatal("an unsubstitutable item must decline")
	}
	badOrder := &ast.Projection{Items: item(&ast.Var{Name: "a"}), OrderBy: []ast.SortItem{{Expr: &ast.Exists{}}}}
	if _, ok := inlineProjection(badOrder, nil, subst); ok {
		t.Fatal("an unsubstitutable ORDER BY key must decline")
	}
	if _, ok := inlineProjection(&ast.Projection{Items: item(&ast.Var{Name: "a"})}, &ast.Exists{}, subst); ok {
		t.Fatal("an unsubstitutable WHERE must decline")
	}
}
