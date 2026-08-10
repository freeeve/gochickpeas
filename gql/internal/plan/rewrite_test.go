package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
)

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

// containsAllPred reports whether e contains an all(...) list predicate.
func containsAllPred(e ast.Expr) bool {
	if e == nil {
		return false
	}
	found := false
	ast.Walk(e, func(x ast.Expr) bool {
		if lp, ok := x.(*ast.ListPred); ok && lp.Quant == ast.QuantAll {
			found = true
		}
		return !found
	})
	return found
}

// TestFusionMonoConjunctConserved pins the pass-interaction invariant
// (task 247): when the projection-fusion pass and the monotonic pushdown
// both have a stake in the same clauses -- a pure LET defining a
// rels-derived alias, then an aggregating boundary whose FILTER carries
// the sortedness conjunct over that alias -- their composition may cost
// an optimization but must never lose or duplicate the predicate. Today
// the composition resolves by fusion failing closed (substExpr has no
// ListPred arm); if either pass learns the shape, this still holds the
// pipeline to the invariant: the conjunct is consumed onto a MonoHop
// spec or survives as a stage/boundary filter -- exactly one home.
func TestFusionMonoConjunctConserved(t *testing.T) {
	g := buildFixture(t)
	one, three := uint64(1), uint64(3)
	pattern := ast.Pattern{
		Start: ast.NodePat{Var: "a", Labels: []string{"Person"}},
		Hops: []ast.PatternHop{{
			Rel:  ast.RelPat{Var: "e", Dir: ast.DirOut, Types: []string{"KNOWS"}, Length: &ast.VarLength{Min: &one, Max: &three}},
			Node: ast.NodePat{Var: "b", Labels: []string{"Person"}},
		}},
	}
	tsComp := &ast.ListComp{Var: "r", List: &ast.Var{Name: "e"}, Map: &ast.Prop{Var: "r", Key: "ts"}}
	idx := func(i ast.Expr) ast.Expr { return &ast.Index{Base: &ast.Var{Name: "ts"}, Idx: i} }
	iVar := &ast.Var{Name: "i"}
	sorted := &ast.ListPred{
		Quant: ast.QuantAll,
		Var:   "i",
		List: &ast.Func{Name: "range", Args: []ast.Expr{
			&ast.Lit{Value: ast.IntLit(0)},
			&ast.Binary{Op: ast.OpSub,
				LHS: &ast.Func{Name: "size", Args: []ast.Expr{&ast.Var{Name: "ts"}}},
				RHS: &ast.Lit{Value: ast.IntLit(2)}},
		}},
		Pred: &ast.Binary{Op: ast.OpLt, LHS: idx(iVar),
			RHS: idx(&ast.Binary{Op: ast.OpAdd, LHS: iVar, RHS: &ast.Lit{Value: ast.IntLit(1)}})},
	}
	q := &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{
			&ast.Match{Patterns: []ast.Pattern{pattern}},
			// Pure boundary defining the alias -- fusion's left operand.
			&ast.With{Proj: ast.Projection{Items: []ast.ReturnItem{
				{Expr: &ast.Var{Name: "b"}, Alias: "b"},
				{Expr: tsComp, Alias: "ts"},
			}}},
			// Aggregating boundary carrying the sortedness conjunct --
			// fusion's right operand and the mono pushdown's source.
			&ast.With{
				Proj: ast.Projection{Items: []ast.ReturnItem{
					{Expr: &ast.Var{Name: "b"}, Alias: "b"},
					{Expr: &ast.Var{Name: "ts"}, Alias: "ts"},
					{Expr: &ast.Func{Name: "count", Star: true}, Alias: "n"},
				}},
				Where: sorted,
			},
		},
		Ret: ast.Projection{Items: []ast.ReturnItem{
			{Expr: &ast.Var{Name: "b"}, Alias: "b"},
			{Expr: &ast.Var{Name: "n"}, Alias: "n"},
		}},
	}}}
	p, err := Build(q, g)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	specs, filters := conjunctHomes(p)
	if specs+filters != 1 {
		t.Fatalf("sortedness conjunct homes = %d spec + %d filter, want exactly 1 -- the fusion x mono composition lost or duplicated the predicate", specs, filters)
	}

	// Keep side (task 254's fixture-scope lesson: the pin above exercises
	// only the push-succeeds home): a comprehension over a non-rels list
	// defeats alias provenance, so the push must FAIL and the conjunct
	// must survive as a filter -- one home, zero specs.
	q.Parts[0].Clauses[1].(*ast.With).Proj.Items[1].Expr = &ast.ListComp{
		Var: "r",
		List: &ast.ListExpr{Elems: []ast.Expr{
			&ast.Lit{Value: ast.IntLit(1)}, &ast.Lit{Value: ast.IntLit(2)}}},
		Map: &ast.Var{Name: "r"},
	}
	p2, err := Build(q, g)
	if err != nil {
		t.Fatalf("keep-side plan: %v", err)
	}
	specs2, filters2 := conjunctHomes(p2)
	if specs2 != 0 || filters2 != 1 {
		t.Fatalf("keep side: %d spec + %d filter homes, want 0 + 1 (unpushable conjunct must survive as a filter)", specs2, filters2)
	}
}

// conjunctHomes counts where the all() sortedness conjunct ended up:
// consumed onto MonoHop specs, or surviving in stage/boundary filters.
func conjunctHomes(p *Plan) (specs, filters int) {
	for _, seg := range p.Branches[0] {
		for _, st := range seg.Stages {
			ms, ok := st.(*MatchStage)
			if !ok {
				continue
			}
			for i := range ms.Ops {
				if ms.Ops[i].MonoHop != nil {
					specs++
				}
			}
			if containsAllPred(ms.Where) {
				filters++
			}
		}
		if containsAllPred(seg.PostWhere) {
			filters++
		}
	}
	return specs, filters
}

// TestFuseTrailingProjectionIntoRet drives the terminal-RETURN arm of the
// fusion: a pure projection boundary directly before an aggregating final
// RETURN folds into it, substituting item, ORDER BY, and alias references,
// without mutating the caller's part (the adaptive sibling rebuild re-plans
// the same AST).
func TestFuseTrailingProjectionIntoRet(t *testing.T) {
	q, err := parser.Parse("MATCH (n:N) RETURN n.flag AS f, n.size AS s NEXT RETURN f, count(s) AS c ORDER BY f DESC LIMIT 5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	part := q.Parts[0]
	clauses, ret := fuseTrailingProjectionIntoRet(part.Clauses, part.Ret)
	if len(clauses) != len(part.Clauses)-1 {
		t.Fatalf("terminal fusion did not fire: %d clauses -> %d", len(part.Clauses), len(clauses))
	}
	byAlias := map[string]ast.Expr{}
	for _, it := range ret.Items {
		byAlias[it.Alias] = it.Expr
	}
	if p, isProp := byAlias["f"].(*ast.Prop); !isProp || p.Var != "n" || p.Key != "flag" {
		t.Fatalf("fused item f = %+v, want Prop(n.flag)", byAlias["f"])
	}
	fn, isFunc := byAlias["c"].(*ast.Func)
	if !isFunc || len(fn.Args) != 1 {
		t.Fatalf("fused item c = %+v, want count(...)", byAlias["c"])
	}
	if p, isProp := fn.Args[0].(*ast.Prop); !isProp || p.Var != "n" || p.Key != "size" {
		t.Fatalf("aggregate arg = %+v, want Prop(n.size)", fn.Args[0])
	}
	if len(ret.OrderBy) != 1 {
		t.Fatalf("ORDER BY lost: %+v", ret.OrderBy)
	}
	if p, isProp := ret.OrderBy[0].Expr.(*ast.Prop); !isProp || p.Var != "n" || p.Key != "flag" {
		t.Fatalf("ORDER BY key = %+v, want Prop(n.flag)", ret.OrderBy[0].Expr)
	}
	if !ret.OrderBy[0].Desc || ret.Limit == nil {
		t.Fatalf("ORDER BY/LIMIT modifiers lost: %+v limit=%v", ret.OrderBy[0], ret.Limit)
	}
	// Non-destructive: the part's own Ret still references the aliases.
	for _, it := range part.Ret.Items {
		if it.Alias == "c" {
			if v, isVar := it.Expr.(*ast.Func).Args[0].(*ast.Var); !isVar || v.Name != "s" {
				t.Fatalf("part.Ret was mutated: count arg = %+v", it.Expr.(*ast.Func).Args[0])
			}
		}
	}
}

// TestFuseProjectionLetChainClauseLevel pins the clause-level fixpoint: a
// chain of LET boundaries before an aggregating mid-query boundary folds
// completely, not just the boundary adjacent to the aggregate.
func TestFuseProjectionLetChainClauseLevel(t *testing.T) {
	q, err := parser.Parse("MATCH (n:N) LET a = n.k LET b = a LET c = n.j RETURN b, count(c) AS m NEXT RETURN b, m")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	clauses := fuseProjectionBeforeAggregate(q.Parts[0].Clauses)
	if len(clauses) != len(q.Parts[0].Clauses)-3 {
		t.Fatalf("LET chain did not fully fold: %d clauses -> %d", len(q.Parts[0].Clauses), len(clauses))
	}
	agg, isWith := clauses[len(clauses)-1].(*ast.With)
	if !isWith || !projectionIsAggregated(&agg.Proj) {
		t.Fatalf("last clause = %T, want the fused aggregating With", clauses[len(clauses)-1])
	}
	for _, it := range agg.Proj.Items {
		switch it.Alias {
		case "b":
			if p, isProp := it.Expr.(*ast.Prop); !isProp || p.Var != "n" || p.Key != "k" {
				t.Fatalf("chained alias b = %+v, want Prop(n.k)", it.Expr)
			}
		case "m":
			if p, isProp := it.Expr.(*ast.Func).Args[0].(*ast.Prop); !isProp || p.Var != "n" || p.Key != "j" {
				t.Fatalf("aggregate arg = %+v, want Prop(n.j)", it.Expr.(*ast.Func).Args[0])
			}
		}
	}
}

// TestFuseTrailingProjectionLetChain pins the star arm and the fixpoint:
// LET lowers to a star projection plus computed aliases, and chained LETs
// stack as successive pure boundaries that all fold into an aggregating
// terminal RETURN.
func TestFuseTrailingProjectionLetChain(t *testing.T) {
	q, err := parser.Parse("MATCH (n:N) LET f = n.flag LET g = f LET h = n.size RETURN g, count(h) AS c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	part := q.Parts[0]
	clauses, ret := fuseTrailingProjectionIntoRet(part.Clauses, part.Ret)
	if len(clauses) != len(part.Clauses)-3 {
		t.Fatalf("LET chain did not fully fold: %d clauses -> %d", len(part.Clauses), len(clauses))
	}
	byAlias := map[string]ast.Expr{}
	for _, it := range ret.Items {
		byAlias[it.Alias] = it.Expr
	}
	// g -> f (second boundary) -> n.flag (first). One fold per iteration.
	if p, isProp := byAlias["g"].(*ast.Prop); !isProp || p.Var != "n" || p.Key != "flag" {
		t.Fatalf("chained alias g = %+v, want Prop(n.flag)", byAlias["g"])
	}
	fn := byAlias["c"].(*ast.Func)
	if p, isProp := fn.Args[0].(*ast.Prop); !isProp || p.Var != "n" || p.Key != "size" {
		t.Fatalf("aggregate arg = %+v, want Prop(n.size)", fn.Args[0])
	}
}

// TestFuseTrailingProjectionIntoRetDeclines pins the guard conditions: no
// fusion for a non-aggregating RETURN, a star boundary, a filtered boundary,
// or an impure (ordered) boundary. Each must return the input unchanged.
func TestFuseTrailingProjectionIntoRetDeclines(t *testing.T) {
	for _, tc := range []struct {
		name, query string
	}{
		{"non-aggregating ret", "MATCH (n:N) RETURN n.flag AS f NEXT RETURN f"},
		{"distinct boundary", "MATCH (n:N) RETURN DISTINCT n.flag AS f NEXT RETURN f, count(*) AS c"},
		{"ordered boundary", "MATCH (n:N) RETURN n.flag AS f ORDER BY f NEXT RETURN count(f) AS c"},
		{"limited boundary", "MATCH (n:N) RETURN n.flag AS f LIMIT 3 NEXT RETURN count(f) AS c"},
		{"ordered star boundary", "MATCH (n:N) ORDER BY n.flag LIMIT 3 RETURN count(n) AS c"},
	} {
		q, err := parser.Parse(tc.query)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		part := q.Parts[0]
		clauses, ret := fuseTrailingProjectionIntoRet(part.Clauses, part.Ret)
		if len(clauses) != len(part.Clauses) {
			t.Errorf("%s: fusion fired (%d clauses -> %d), must decline", tc.name, len(part.Clauses), len(clauses))
		}
		if len(ret.Items) != len(part.Ret.Items) {
			t.Errorf("%s: ret rewritten despite decline", tc.name)
		}
	}
	// A filtered boundary (With.Where != nil) has no surface form ending in
	// NEXT here, so build it directly.
	w := &ast.With{
		Proj:  ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Prop{Var: "n", Key: "flag"}, Alias: "f"}}},
		Where: &ast.IsNull{Expr: &ast.Var{Name: "f"}, Negated: true},
	}
	ret := ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Func{Name: "count", Args: []ast.Expr{&ast.Var{Name: "f"}}}, Alias: "c"}}}
	clauses, _ := fuseTrailingProjectionIntoRet([]ast.Clause{w}, ret)
	if len(clauses) != 1 {
		t.Error("filtered boundary: fusion fired, must decline (the filter would be dropped)")
	}
}
