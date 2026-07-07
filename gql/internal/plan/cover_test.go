// Coverage of the cost probes' abstention rules, nested-aggregate
// extraction arms, estimate edge shapes, and anchor tie-breaks.
package plan

import (
	"math"
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

func TestAnchorCardAbstainsOnParams(t *testing.T) {
	g := buildFixture(t)
	// A param-valued inline property must NOT be probed: the cardinality
	// falls back to the label size, so an autoparam'd template plans
	// value-independently (plan-cache safety).
	n := &ast.NodePat{Var: "tg", Labels: []string{"Tag"}, Props: []ast.PropEntry{{Key: "name", Val: ast.ParamLit(0)}}}
	slots := map[string]int{}
	bound := map[int]bool{}
	if got := anchorCard(n, nil, slots, bound, g); got != 4 {
		t.Fatalf("param anchor card = %d, want the Tag label cardinality 4", got)
	}
	// A concrete value probes the posting length.
	n.Props[0].Val = ast.StrLit("tagA")
	if got := anchorCard(n, nil, slots, bound, g); got != 1 {
		t.Fatalf("concrete anchor card = %d, want 1", got)
	}
	// Bound -> 0; id seek -> 1; unlabelled -> node count.
	slots["tg"] = 0
	bound[0] = true
	if got := anchorCard(n, nil, slots, bound, g); got != 0 {
		t.Fatalf("bound anchor card = %d", got)
	}
	free := &ast.NodePat{Var: "x"}
	idWhere := &ast.Binary{Op: ast.OpEq,
		LHS: &ast.Func{Name: "id", Args: []ast.Expr{&ast.Var{Name: "x"}}},
		RHS: &ast.Lit{Value: ast.IntLit(3)}}
	if got := anchorCard(free, idWhere, map[string]int{}, map[int]bool{}, g); got != 1 {
		t.Fatalf("id-seek anchor card = %d", got)
	}
	if got := anchorCard(&ast.NodePat{Var: "y"}, nil, map[string]int{}, map[int]bool{}, g); got != 64 {
		t.Fatalf("unlabelled anchor card = %d, want the node count 64", got)
	}
	// resolveAnchorNodes abstains on params and bound vars too.
	if _, ok := resolveAnchorNodes(n, nil, slots, bound, g); ok {
		t.Fatal("bound node must not resolve")
	}
	pn := &ast.NodePat{Var: "z", Labels: []string{"Tag"}, Props: []ast.PropEntry{{Key: "name", Val: ast.NamedParamLit("p")}}}
	if _, ok := resolveAnchorNodes(pn, nil, map[string]int{}, map[int]bool{}, g); ok {
		t.Fatal("param-only node must not resolve")
	}
	if _, ok := resolveAnchorNodes(free, idWhere, map[string]int{}, map[int]bool{}, g); !ok {
		t.Fatal("an int id seek resolves")
	}
}

func TestResolvedFirstHopDegree(t *testing.T) {
	g := buildFixture(t)
	// tagA carries 5 of the 20 HAS_TAG rels (i%4==0).
	p := &ast.Pattern{
		Start: ast.NodePat{Var: "tg", Labels: []string{"Tag"}, Props: []ast.PropEntry{{Key: "name", Val: ast.StrLit("tagA")}}},
		Hops: []ast.PatternHop{{
			Rel:  ast.RelPat{Dir: ast.DirIn, Types: []string{"HAS_TAG"}},
			Node: ast.NodePat{Var: "m", Labels: []string{"Message"}},
		}},
	}
	d, ok := resolvedFirstHopDegree(p, nil, map[string]int{}, map[int]bool{}, g)
	if !ok || d != 5 {
		t.Fatalf("degree = %d/%v, want 5", d, ok)
	}
	// Untyped hop counts all neighbors.
	p.Hops[0].Rel.Types = nil
	d, ok = resolvedFirstHopDegree(p, nil, map[string]int{}, map[int]bool{}, g)
	if !ok || d != 5 {
		t.Fatalf("untyped degree = %d/%v", d, ok)
	}
	// No hops -> no degree.
	if _, ok := resolvedFirstHopDegree(&ast.Pattern{Start: p.Start}, nil, map[string]int{}, map[int]bool{}, g); ok {
		t.Fatal("hopless pattern has no first-hop degree")
	}
}

func TestNestedAggExtractionArms(t *testing.T) {
	g := buildFixture(t)
	// Exercise the CASE / list / IN / IS NULL / index / slice / propOf /
	// unary recursion arms of extractNestedAggs in one projection.
	src := "MATCH (m:Message) RETURN " +
		"CASE WHEN sum(m.len) > 10 THEN sum(m.len) ELSE 0 END AS a, " +
		"[sum(m.len), count(*)][0] AS b, " +
		"(sum(m.len) IN [1, 2]) AS c, " +
		"(sum(m.len) IS NULL) AS d, " +
		"-sum(m.len) AS e, " +
		"abs(sum(m.len)) AS f, " +
		"[sum(m.len), 0][0..1] AS gg"
	p := mustPlan(t, g, src)
	proj := &p.Branches[0][0].Proj
	if len(proj.Post) != 7 || proj.NHidden < 7 {
		t.Fatalf("post = %d, hidden = %d", len(proj.Post), proj.NHidden)
	}
	// An aggregate inside an unsupported position (a list predicate's
	// body) errors.
	planErr(t, g, "MATCH (m:Message) RETURN all(x IN [1] WHERE sum(m.len) > 0) AS bad",
		"aggregate here is not supported")
}

func TestUnwindListEstimateAndScanArms(t *testing.T) {
	g := buildFixture(t)
	// ScanAll estimate, rebind expand estimate (cycle), and prop-narrowed
	// label scan estimate.
	p := mustPlan(t, g, "MATCH (a:Person {pid: 1})-[:KNOWS]->(b:Person)-[:KNOWS]->(a) RETURN b.pid")
	est := Estimate(p, g)
	m := est.Segs[0].Stages[0].Match
	if len(m) == 0 {
		t.Fatal("no op estimates")
	}
	// The final rebind hop cannot exceed its input rows.
	if m[len(m)-1] > m[len(m)-2] {
		t.Fatalf("rebind estimate grew: %v", m)
	}
	if rnd(math.Inf(1)) != math.MaxUint64 || rnd(-3) != 0 || rnd(math.NaN()) != 0 {
		t.Fatal("rnd saturation")
	}
	if propSel(nil) != 1 {
		t.Fatal("no props -> selectivity 1")
	}
}

func TestValueFieldNamesAndLitValue(t *testing.T) {
	for kind, want := range map[ProcKind]string{
		ProcWcc: "component", ProcBfs: "value", ProcPageRank: "value",
		ProcWccAll: "value", ProcCdlp: "value", ProcLcc: "value",
		ProcSssp: "value", ProcFtsSearch: "", ProcGeoWithinRadius: "", ProcGeoWithinBBox: "",
	} {
		if got := valueFieldName(&CallProc{Kind: kind}); got != want {
			t.Fatalf("valueFieldName(%d) = %q, want %q", kind, got, want)
		}
	}
	if !semantics.LitValue(ast.BoolLit(true)).IsTruthy() {
		t.Fatal("LitValue bool")
	}
}

func TestIDSeekVariantsAndErrors(t *testing.T) {
	g := buildFixture(t)
	// Reversed operand order (`3 = id(n)`), and a $named param seek.
	p := mustPlan(t, g, "MATCH (n) WHERE 3 = id(n) RETURN n")
	if firstMatch(t, p).Ops[0].Source.Kind != ScanNodeID {
		t.Fatal("reversed id seek not recognized")
	}
	p = mustPlan(t, g, "MATCH (n) WHERE id(n) = $seed RETURN n")
	if firstMatch(t, p).Ops[0].Source.Kind != ScanNodeID {
		t.Fatal("param id seek not recognized")
	}
	// A non-integer literal is not a seek.
	p = mustPlan(t, g, "MATCH (n) WHERE id(n) = 'x' RETURN n")
	if firstMatch(t, p).Ops[0].Source.Kind != ScanAll {
		t.Fatal("string id equality must stay a scan-filter")
	}
	// Inline relationship properties are rejected.
	planErr(t, g, "MATCH (a:Person)-[:KNOWS {w: 1}]->(b) RETURN b", "inline relationship properties")
	// A named path over a quantified single hop binds through RelsSlot.
	p = mustPlan(t, g, "MATCH pth = (a:Person {pid: 1})-[:KNOWS]->{1,2}(b) RETURN pth")
	ms := firstMatch(t, p)
	if ms.PathBind == nil || ms.Ops[1].RelSlot != ms.PathBind.RelsSlot {
		t.Fatal("path bind over a quantified hop keeps the hidden rel slot")
	}
}

func TestUnionAllBranchesAndOrderByScope(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (tg:Tag) RETURN tg.name AS n UNION ALL MATCH (p:Person) RETURN 'p' AS n UNION MATCH (m:Message) RETURN 'm' AS n")
	if len(p.Branches) != 3 || len(p.Union) != 2 {
		t.Fatalf("branches = %d unions = %d", len(p.Branches), len(p.Union))
	}
	if p.Union[0] != ast.UnionAll || p.Union[1] != ast.UnionDistinct {
		t.Fatalf("union kinds = %v", p.Union)
	}
	// ORDER BY a non-projected variable is fine without aggregation...
	mustPlan(t, g, "MATCH (m:Message) RETURN m.len AS l ORDER BY m.pid")
	// ...but must reference a projection column when aggregating.
	planErr(t, g, "MATCH (m:Message) RETURN count(*) AS n ORDER BY m.len", "must reference a projection column")
	// ...and under DISTINCT, where a discarded variable is ambiguous per
	// surviving row.
	planErr(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN DISTINCT tg.name AS n ORDER BY m.len", "must reference a projection column")
	// A key over the projected variables stays legal with DISTINCT.
	mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN DISTINCT tg ORDER BY tg.name")
	// RETURN * with nothing in scope errors.
	planErr(t, g, "RETURN *", "at least one variable in scope")
	if strings.Contains(mustPlanColumns(t, g, "MATCH (a:Person) MATCH (b:Message) RETURN *"), "b, a") {
		t.Fatal("star projects in slot order")
	}
}

// mustPlanColumns plans and returns the joined output column names.
func mustPlanColumns(t *testing.T, g interface {
	NodeCount() uint32
}, src string) string {
	t.Helper()
	gg := g.(interface{ NodeCount() uint32 })
	_ = gg
	return strings.Join(mustPlan(t, buildFixture(t), src).Columns, ", ")
}

func TestCollectAllVarsArms(t *testing.T) {
	// Every expression arm feeds the reorder's correlation guard.
	out := map[string]bool{}
	e := &ast.Case{
		Operand: &ast.Var{Name: "a"},
		Whens: []ast.CaseWhen{{
			Cond:   &ast.ListPred{Quant: ast.QuantAll, Var: "x", List: &ast.Var{Name: "b"}, Pred: &ast.Prop{Var: "x", Key: "k"}},
			Result: &ast.Reduce{Acc: "acc", Init: &ast.Var{Name: "c"}, Var: "r", List: &ast.Var{Name: "d"}, Body: &ast.Var{Name: "acc"}},
		}},
		Else: &ast.ListComp{Var: "y", List: &ast.Var{Name: "e"}, Filter: &ast.Var{Name: "f"}, Map: &ast.Var{Name: "g"}},
	}
	collectAllVars(e, out)
	for _, v := range []string{"a", "b", "c", "d", "e", "f", "g", "x", "y", "acc", "r"} {
		if !out[v] {
			t.Fatalf("missing %s in %v", v, out)
		}
	}
	out = map[string]bool{}
	collectAllVars(&ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: &ast.Var{Name: "h"}}}}, out)
	collectAllVars(&ast.MapProj{Var: "i", Entries: []ast.MapProjEntry{{Kind: ast.MapProjField, Key: "f", Expr: &ast.Var{Name: "j"}}}}, out)
	collectAllVars(&ast.Cost{From: "k", To: "l"}, out)
	collectAllVars(&ast.Slice{Base: &ast.Var{Name: "m"}, From: &ast.Var{Name: "n"}, To: &ast.Var{Name: "o"}}, out)
	collectAllVars(&ast.PropOf{Base: &ast.Var{Name: "p"}, Key: "z"}, out)
	collectAllVars(&ast.CountSub{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "q"}}}, out)
	collectAllVars(&ast.PatternComp{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "r2"}}, Proj: &ast.Var{Name: "s"}}, out)
	collectAllVars(&ast.Index{Base: &ast.Var{Name: "t2"}, Idx: &ast.Var{Name: "u"}}, out)
	collectAllVars(&ast.IsNull{Expr: &ast.Var{Name: "v"}}, out)
	collectAllVars(&ast.HasLabelExpr{Var: "w"}, out)
	for _, v := range []string{"h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r2", "s", "t2", "u", "v", "w"} {
		if !out[v] {
			t.Fatalf("missing %s", v)
		}
	}
}

func TestPredRefsOnlyArms(t *testing.T) {
	ok := func(e ast.Expr) bool { return predRefsOnly(e, "r") == nil }
	if !ok(&ast.In{Expr: &ast.Prop{Var: "r", Key: "k"}, List: &ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}}}) {
		t.Fatal("IN over r accepted")
	}
	if !ok(&ast.Func{Name: "abs", Args: []ast.Expr{&ast.Unary{Op: ast.Neg, Expr: &ast.Prop{Var: "r", Key: "k"}}}}) {
		t.Fatal("func/unary over r accepted")
	}
	if !ok(&ast.IsNull{Expr: &ast.Var{Name: "r"}}) {
		t.Fatal("IS NULL over r accepted")
	}
	if ok(&ast.Prop{Var: "other", Key: "k"}) || ok(&ast.Var{Name: "other"}) {
		t.Fatal("foreign var rejected")
	}
	// The free-variable check sees through binders: a comprehension over r
	// with a local iteration variable passes, a correlated WHERE on an
	// outer variable does not.
	if !ok(&ast.ListComp{Var: "x", List: &ast.ListExpr{Elems: []ast.Expr{&ast.Prop{Var: "r", Key: "k"}}}, Map: &ast.Var{Name: "x"}}) {
		t.Fatal("comprehension over r accepted")
	}
	if ok(&ast.Exists{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "x"}}, Where: &ast.Prop{Var: "outer", Key: "k"}}) {
		t.Fatal("correlated foreign reference rejected")
	}
}

func TestExtractAggErrors(t *testing.T) {
	g := buildFixture(t)
	planErr(t, g, "MATCH (m:Message) RETURN sum(*) AS s", "is not valid")
	planErr(t, g, "MATCH (m:Message) RETURN percentile(m.len) AS p", "unknown")
	// min/max/avg/collect kinds all bind.
	p := mustPlan(t, g, "MATCH (m:Message) RETURN min(m.len) AS a, max(m.len) AS b, avg(m.len) AS c, collect(m.len) AS d, sum(m.len) AS e")
	if got := len(p.Branches[0][0].Proj.Aggs); got != 5 {
		t.Fatalf("aggs = %d", got)
	}
}

func TestEstimateScanArms(t *testing.T) {
	g := buildFixture(t)
	// Param property seek: estimate falls back to label cardinality.
	p := mustPlan(t, g, "MATCH (tg:Tag {name: $t})-[:HAS_TAG]-(m) RETURN m")
	est := Estimate(p, g)
	if est.Segs[0].Stages[0].Match[0] != 4 {
		t.Fatalf("param seek est = %d, want the Tag label size 4", est.Segs[0].Stages[0].Match[0])
	}
	// Text-match scan estimates at label size; extra inline props narrow.
	p = mustPlan(t, g, "MATCH (tg:Tag) WHERE tg.name STARTS WITH 'tag' RETURN tg")
	est = Estimate(p, g)
	if est.Segs[0].Stages[0].Match[0] != 4 {
		t.Fatalf("text-match est = %d", est.Segs[0].Stages[0].Match[0])
	}
}
