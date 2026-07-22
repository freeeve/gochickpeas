package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// Planner tests for named-path derived monotonic pushdown, shortest-path
// per-hop predicates and their errors, the weight-expression checker's
// arms, and projection-fusion substitution. Split from mono_test.go (which
// keeps the monotonic-pushdown fixtures/helpers and the core mono tests).

func TestDerivedMonoViaNamedPath(t *testing.T) {
	g := buildFixture(t)
	// rels(p) over a single-quantified-hop named path resolves through the
	// path's hidden rel slot.
	q := derivedMonoQuery(false)
	// Rewrite the fixture query to bind a named path and comprehend over
	// rels(p) instead of the rel variable.
	m := q.Parts[0].Clauses[0].(*ast.Match)
	pb := &ast.PathBind{PathVar: "p", Pattern: m.Patterns[0]}
	pb.Pattern.Hops[0].Rel.Var = ""
	q.Parts[0].Clauses[0] = pb
	w := q.Parts[0].Clauses[1].(*ast.With)
	w.Proj.Items[1].Expr.(*ast.ListComp).List = &ast.Func{Name: "rels", Args: []ast.Expr{&ast.Var{Name: "p"}}}
	p, err := Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	ms := firstMatch(t, p)
	if ms.Ops[1].MonoHop == nil {
		t.Fatal("named-path derived mono must push through the rel slot")
	}
}

func TestSpPerHopPredAndErrors(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[e:KNOWS]->{1,4}(b) WHERE all(r IN rels(e) WHERE r.w > 0) RETURN pth")
	var sp *SpStage
	for _, s := range p.Branches[0][0].Stages {
		if v, ok := s.(*SpStage); ok {
			sp = v
		}
	}
	if sp == nil || sp.RelPred == nil || sp.RelPred.Var != "r" {
		t.Fatalf("sp rel pred = %+v", sp)
	}
	planErr(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[e:KNOWS]->{1,4}(b) WHERE a.pid = 1 RETURN pth",
		"only supported as")
	planErr(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[e:KNOWS]->{1,4}(b) WHERE any(r IN rels(e) WHERE r.w > 0) RETURN pth",
		"only `all(")
	planErr(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[:KNOWS]->{1,4}(b)-[:KNOWS]->(a) RETURN pth",
		"exactly one relationship")
}

func TestWeightExprArms(t *testing.T) {
	// Exercise the weight checker's expression arms directly.
	r := func(k string) ast.Expr { return &ast.Prop{Var: "r", Key: k} }
	okExprs := []ast.Expr{
		&ast.Case{Whens: []ast.CaseWhen{{Cond: &ast.Binary{Op: ast.OpGt, LHS: r("w"), RHS: &ast.Lit{Value: ast.IntLit(0)}}, Result: r("w")}}, Else: &ast.Lit{Value: ast.FloatLit(1)}},
		&ast.In{Expr: r("k"), List: &ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}}},
		&ast.Index{Base: &ast.ListExpr{Elems: []ast.Expr{r("w")}}, Idx: &ast.Lit{Value: ast.IntLit(0)}},
		&ast.Slice{Base: &ast.ListExpr{Elems: []ast.Expr{r("w")}}, From: &ast.Lit{Value: ast.IntLit(0)}, To: &ast.Lit{Value: ast.IntLit(1)}},
		&ast.IsNull{Expr: r("w")},
		&ast.PropOf{Base: &ast.Var{Name: "r"}, Key: "w"},
		&ast.Func{Name: "abs", Args: []ast.Expr{&ast.Unary{Op: ast.Neg, Expr: r("w")}}},
		// A correlated COUNT subquery whose only free var is r.
		&ast.CountSub{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "x"}}, Where: &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "k"}, RHS: r("k")}},
		// A reduce whose accumulator/iteration vars are locals: the
		// free-variable check accepts any form whose free refs are only r.
		&ast.Reduce{Acc: "a", Init: r("w"), Var: "v", List: &ast.ListExpr{Elems: []ast.Expr{r("w")}}, Body: &ast.Var{Name: "a"}},
	}
	for i, e := range okExprs {
		if err := validateWeightExpr(e, []string{"r"}); err != nil {
			t.Fatalf("okExprs[%d]: %v", i, err)
		}
	}
	badExprs := []ast.Expr{
		&ast.Var{Name: "other"},
		&ast.Exists{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "x"}}, Where: &ast.Prop{Var: "outer", Key: "k"}},
		&ast.Reduce{Acc: "a", Init: &ast.Var{Name: "outer"}, Var: "v", List: r("w"), Body: &ast.Var{Name: "v"}},
	}
	for i, e := range badExprs {
		if err := validateWeightExpr(e, []string{"r"}); err == nil {
			t.Fatalf("badExprs[%d] accepted", i)
		}
	}
}

func TestFusionSubstArms(t *testing.T) {
	g := buildFixture(t)
	// The fused alias flows through CASE / IN / list / IS NULL / label
	// tests; a Prop on a renamed bare variable rewrites; ORDER BY keys
	// substitute too.
	p := mustPlan(t, g, "MATCH (m:Message) RETURN m AS msg, m.len AS l NEXT RETURN msg.len AS ml, CASE WHEN l IN [10, 20] THEN 1 ELSE 0 END AS flag, count(*) AS n ORDER BY ml")
	if got := len(p.Branches[0]); got != 2 {
		t.Fatalf("segments = %d, want the rename fused into the aggregate", got)
	}
	// A Prop over a computed (non-variable) alias abandons the fusion.
	p = mustPlan(t, g, "MATCH (m:Message) RETURN {k: m.len} AS obj NEXT RETURN obj.k AS v, count(*) AS n NEXT RETURN v, n")
	if got := len(p.Branches[0]); got != 3 {
		t.Fatalf("segments = %d, want no fusion over a map alias", got)
	}
}
