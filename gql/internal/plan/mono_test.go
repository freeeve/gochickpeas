// Monotonic pushdown, fusion, reorder-correlation, label-expression, and
// weight-validation tests. The projection-derived mono forms have no GQL
// surface (list comprehensions are engine-only), so those build ASTs
// directly.
package plan

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// derivedMonoQuery hand-builds: MATCH p = (a:Person)-[:KNOWS]->{1,3}(b)
// WITH b, [r IN rels(p) | r.ts] AS ts WHERE all(i IN range(0, size(ts)-2)
// WHERE ts[i] < ts[i+1]) RETURN b -- the projection-derived mono form.
func derivedMonoQuery(violationForm bool) *ast.Query {
	one, three := uint64(1), uint64(3)
	pattern := ast.Pattern{
		Start: ast.NodePat{Var: "a", Labels: []string{"Person"}},
		Hops: []ast.PatternHop{{
			Rel:  ast.RelPat{Var: "e", Dir: ast.DirOut, Types: []string{"KNOWS"}, Length: &ast.VarLength{Min: &one, Max: &three}},
			Node: ast.NodePat{Var: "b", Labels: []string{"Person"}},
		}},
	}
	tsComp := &ast.ListComp{
		Var:  "r",
		List: &ast.Var{Name: "e"},
		Map:  &ast.Prop{Var: "r", Key: "ts"},
	}
	idx := func(i ast.Expr) ast.Expr { return &ast.Index{Base: &ast.Var{Name: "ts"}, Idx: i} }
	iVar := &ast.Var{Name: "i"}
	plusOne := &ast.Binary{Op: ast.OpAdd, LHS: iVar, RHS: &ast.Lit{Value: ast.IntLit(1)}}
	minusOne := &ast.Binary{Op: ast.OpSub, LHS: iVar, RHS: &ast.Lit{Value: ast.IntLit(1)}}
	sizeTs := &ast.Func{Name: "size", Args: []ast.Expr{&ast.Var{Name: "ts"}}}
	var where ast.Expr
	if violationForm {
		// size([i IN range(1, size(ts)) WHERE ts[i-1] >= ts[i]]) = 0
		where = &ast.Binary{
			Op: ast.OpEq,
			LHS: &ast.Func{Name: "size", Args: []ast.Expr{&ast.ListComp{
				Var:    "i",
				List:   &ast.Func{Name: "range", Args: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}, sizeTs}},
				Filter: &ast.Binary{Op: ast.OpGte, LHS: idx(minusOne), RHS: idx(iVar)},
			}}},
			RHS: &ast.Lit{Value: ast.IntLit(0)},
		}
	} else {
		// all(i IN range(0, size(ts) - 2) WHERE ts[i] < ts[i+1])
		where = &ast.ListPred{
			Quant: ast.QuantAll,
			Var:   "i",
			List: &ast.Func{Name: "range", Args: []ast.Expr{
				&ast.Lit{Value: ast.IntLit(0)},
				&ast.Binary{Op: ast.OpSub, LHS: sizeTs, RHS: &ast.Lit{Value: ast.IntLit(2)}},
			}},
			Pred: &ast.Binary{Op: ast.OpLt, LHS: idx(iVar), RHS: idx(plusOne)},
		}
	}
	return &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{
			&ast.Match{Patterns: []ast.Pattern{pattern}},
			&ast.With{
				Proj: ast.Projection{Items: []ast.ReturnItem{
					{Expr: &ast.Var{Name: "b"}, Alias: "b"},
					{Expr: tsComp, Alias: "ts"},
				}},
				Where: where,
			},
		},
		Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Var{Name: "b"}, Alias: "b"}}},
	}}}
}

func TestDerivedMonoPushdownBothForms(t *testing.T) {
	g := buildFixture(t)
	for _, violation := range []bool{false, true} {
		p, err := Build(derivedMonoQuery(violation), g)
		if err != nil {
			t.Fatalf("violation=%v: %v", violation, err)
		}
		ms := firstMatch(t, p)
		ve := &ms.Ops[1]
		if ve.MonoHop == nil || ve.MonoHop.RelKey != "ts" || !ve.MonoHop.Ascending {
			t.Fatalf("violation=%v: mono = %+v, want ascending ts", violation, ve.MonoHop)
		}
		// The post-filter stays in place (redundant, guards correctness).
		if p.Branches[0][0].PostWhere == nil {
			t.Fatalf("violation=%v: post-where must remain", violation)
		}
	}
}

// crossSegmentMonoQuery builds the CR1 shape where the LET projection and
// the FILTER are separate clauses, so they lower into adjacent segments:
// MATCH p = (a)-[:KNOWS]->{1,3}(b) WITH b, [r IN rels(p) | r.ts] AS ts
// (no where) WITH b, ts WHERE all(i IN range(0,size(ts)-2) WHERE ts[i] <
// ts[i+1]) RETURN b.
func crossSegmentMonoQuery() *ast.Query {
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
	plusOne := &ast.Binary{Op: ast.OpAdd, LHS: iVar, RHS: &ast.Lit{Value: ast.IntLit(1)}}
	sizeTs := &ast.Func{Name: "size", Args: []ast.Expr{&ast.Var{Name: "ts"}}}
	where := &ast.ListPred{
		Quant: ast.QuantAll,
		Var:   "i",
		List: &ast.Func{Name: "range", Args: []ast.Expr{
			&ast.Lit{Value: ast.IntLit(0)},
			&ast.Binary{Op: ast.OpSub, LHS: sizeTs, RHS: &ast.Lit{Value: ast.IntLit(2)}},
		}},
		Pred: &ast.Binary{Op: ast.OpLt, LHS: idx(iVar), RHS: idx(plusOne)},
	}
	return &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{
			&ast.Match{Patterns: []ast.Pattern{pattern}},
			// LET: project ts, no filter -- ends one segment.
			&ast.With{Proj: ast.Projection{Items: []ast.ReturnItem{
				{Expr: &ast.Var{Name: "b"}, Alias: "b"},
				{Expr: tsComp, Alias: "ts"},
			}}},
			// FILTER: passthrough projection with the monotonic where -- a
			// second segment that no longer holds the var-expand.
			&ast.With{
				Proj:  ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Var{Name: "b"}, Alias: "b"}, {Expr: &ast.Var{Name: "ts"}, Alias: "ts"}}},
				Where: where,
			},
		},
		Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Var{Name: "b"}, Alias: "b"}}},
	}}}
}

func TestCrossSegmentMonoPushdown(t *testing.T) {
	g := buildFixture(t)
	p, err := Build(crossSegmentMonoQuery(), g)
	if err != nil {
		t.Fatal(err)
	}
	// The var-expand lives in the first segment; the filter in a later one.
	ms := firstMatch(t, p)
	var ve *BindOp
	for i := range ms.Ops {
		if ms.Ops[i].Kind == OpVarExpand {
			ve = &ms.Ops[i]
		}
	}
	if ve == nil || ve.MonoHop == nil || ve.MonoHop.RelKey != "ts" || !ve.MonoHop.Ascending {
		t.Fatalf("cross-segment mono = %+v, want ascending ts pushed onto the var-expand", ve)
	}
	// The lone monotonic conjunct is consumed -- the walk pruning emits
	// exactly the filtered set, so the post-filter would remove nothing
	// (see TestCrossSegmentMonoDropCorrectness in the gql package).
	for _, segs := range p.Branches {
		for _, s := range segs {
			if s.PostWhere != nil {
				t.Fatalf("cross-segment mono conjunct must be consumed, PostWhere = %v", s.PostWhere)
			}
		}
	}
}

func TestProjectionFusionFires(t *testing.T) {
	g := buildFixture(t)
	// RETURN..NEXT (pure, aliased) then RETURN..NEXT (aggregating): the
	// pure projection inlines into the aggregate, saving a segment.
	p := mustPlan(t, g, "MATCH (m:Message) RETURN m.len / 50 AS bucket NEXT RETURN bucket, count(*) AS n NEXT RETURN n ORDER BY n")
	if got := len(p.Branches[0]); got != 2 {
		t.Fatalf("segments = %d, want 2 (the pure projection fused into the aggregate)", got)
	}
	agg := p.Branches[0][0].Proj
	if !agg.Aggregated {
		t.Fatal("first segment should aggregate after fusion")
	}
	// The group key is the inlined m.len / 50, still named bucket.
	if agg.Columns[0] != "bucket" {
		t.Fatalf("columns = %v", agg.Columns)
	}
	if _, ok := agg.Returns[0].Expr.(*ast.Binary); !ok {
		t.Fatalf("group expr = %T, want the inlined division", agg.Returns[0].Expr)
	}
}

func TestReorderKeepsCorrelatedWhereAfterBinding(t *testing.T) {
	g := buildFixture(t)
	// The selective seek pattern's WHERE correlates on b (bound by the
	// FIRST written pattern): moving it first would flip the EXISTS from
	// correlated to uncorrelated, so the reorder must keep b's scan first.
	p := mustPlan(t, g, "MATCH (b:Person) MATCH (a:Person {pid: 3}) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) } RETURN b.pid")
	seg := p.Branches[0][0]
	ms := seg.Stages[0].(*MatchStage)
	if ms.Ops[0].Source.Kind != ScanLabel || ms.Ops[0].Source.Label != "Person" {
		t.Fatalf("first anchor = %+v, want the b label scan (correlation guard)", ms.Ops[0].Source)
	}
	if len(ms.Ops) != 1 {
		t.Fatalf("first stage ops = %d, want the bare b scan", len(ms.Ops))
	}
}

func TestLabelExpressionLowering(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (n:Person|Message) RETURN n")
	ms := firstMatch(t, p)
	hle, ok := ms.Where.(*ast.HasLabelExpr)
	if !ok || hle.Var != "n" || hle.Expr.Kind != ast.LabelOr {
		t.Fatalf("where = %#v, want a HasLabelExpr OR conjunct", ms.Where)
	}
	// The scan falls back to all-nodes (no plain conjunctive label).
	if ms.Ops[0].Source.Kind != ScanAll {
		t.Fatalf("scan = %+v", ms.Ops[0].Source)
	}
	planErr(t, g, "MATCH (:Person|Message) RETURN 1", "requires a variable")
}

func TestWeightExprValidation(t *testing.T) {
	g := buildFixture(t)
	one := uint64(1)
	mk := func(weight *ast.CostSpec, relVar string) *ast.Query {
		return &ast.Query{Parts: []ast.QueryPart{{
			Clauses: []ast.Clause{
				&ast.Match{Patterns: []ast.Pattern{
					{Start: ast.NodePat{Var: "a", Labels: []string{"Person"}, Props: []ast.PropEntry{{Key: "pid", Val: ast.IntLit(1)}}}},
					{Start: ast.NodePat{Var: "b", Labels: []string{"Person"}, Props: []ast.PropEntry{{Key: "pid", Val: ast.IntLit(5)}}}},
				}},
				&ast.ShortestPath{
					PathVar: "pth",
					Pattern: ast.Pattern{
						Start: ast.NodePat{Var: "a"},
						Hops: []ast.PatternHop{{
							Rel:  ast.RelPat{Var: relVar, Dir: ast.DirOut, Types: []string{"KNOWS"}, Length: &ast.VarLength{Min: &one}},
							Node: ast.NodePat{Var: "b"},
						}},
					},
					Weight: weight,
				},
			},
			Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Var{Name: "pth"}, Alias: "pth"}}},
		}}}
	}
	// A per-edge weight over the rel variable is accepted.
	w := &ast.CostSpec{Kind: ast.CostExpr, Expr: &ast.Binary{Op: ast.OpDiv, LHS: &ast.Lit{Value: ast.FloatLit(1)}, RHS: &ast.Prop{Var: "r", Key: "w"}}}
	if _, err := Build(mk(w, "r"), g); err != nil {
		t.Fatalf("valid weight rejected: %v", err)
	}
	// Referencing anything else is rejected.
	bad := &ast.CostSpec{Kind: ast.CostExpr, Expr: &ast.Prop{Var: "a", Key: "pid"}}
	if _, err := Build(mk(bad, "r"), g); err == nil || !strings.Contains(err.Error(), "may reference only") {
		t.Fatalf("err = %v", err)
	}
	// A weight expression without a named relationship is rejected.
	if _, err := Build(mk(w, ""), g); err == nil || !strings.Contains(err.Error(), "named relationship") {
		t.Fatalf("err = %v", err)
	}
	// ALL SHORTEST + weight is rejected.
	q := mk(w, "r")
	q.Parts[0].Clauses[1].(*ast.ShortestPath).All = true
	if _, err := Build(q, g); err == nil || !strings.Contains(err.Error(), "weighted") {
		t.Fatalf("err = %v", err)
	}
}

func TestEstimateShapes(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) RETURN m.len LIMIT 2")
	est := Estimate(p, g)
	if len(est.Segs) != 1 {
		t.Fatalf("segs = %d", len(est.Segs))
	}
	se := est.Segs[0]
	if se.ProjRows == nil || *se.ProjRows > 2 {
		t.Fatalf("proj rows = %v, want clamped by LIMIT 2", se.ProjRows)
	}
	if len(se.Stages) != 1 || len(se.Stages[0].Match) != 2 {
		t.Fatalf("stage ests = %+v, want scan + expand", se.Stages)
	}
	if se.Stages[0].Match[0] != 1 {
		t.Fatalf("anchor est = %d, want 1 (property seek)", se.Stages[0].Match[0])
	}
	if se.AnchorNotes[0] == "" {
		t.Fatal("anchor note expected for a two-endpoint choice")
	}
	// Aggregated projection has no row estimate; boundary WHERE halves.
	p = mustPlan(t, g, "MATCH (m:Message) RETURN count(*) AS n NEXT FILTER n > 1 RETURN n")
	est = Estimate(p, g)
	if est.Segs[0].ProjRows != nil {
		t.Fatal("aggregated segment estimates no group count")
	}
	// FOR fan-out and unbounded reach cap.
	p = mustPlan(t, g, "FOR x IN [1, 2] MATCH (a:Person {pid: 1})-[:KNOWS]->*(b:Person) RETURN b.pid")
	est = Estimate(p, g)
	if est.Segs[0].Stages[0].Single == nil {
		t.Fatal("FOR stage estimate missing")
	}
	last := est.Segs[0].Stages[1].Match
	if last[len(last)-1] > 40*4+1 {
		t.Fatalf("reach estimate %d not capped by label population", last[len(last)-1])
	}
	if GroupDigits(2860664) != "2,860,664" {
		t.Fatal("GroupDigits")
	}
}

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
	}
	for i, e := range okExprs {
		if err := validateWeightExpr(e, []string{"r"}); err != nil {
			t.Fatalf("okExprs[%d]: %v", i, err)
		}
	}
	badExprs := []ast.Expr{
		&ast.Var{Name: "other"},
		&ast.Exists{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "x"}}, Where: &ast.Prop{Var: "outer", Key: "k"}},
		&ast.Reduce{Acc: "a", Init: r("w"), Var: "v", List: r("w"), Body: r("w")},
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
