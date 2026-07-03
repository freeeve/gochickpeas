// Formatting-helper tables: literal/expression/label/operator rendering
// and the CALL procedure labels, plus a hand-built Profile zip check.
package explain

import (
	"strings"
	"testing"
	"time"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

func lit(l ast.Literal) *ast.Lit { return &ast.Lit{Value: l} }

func TestFmtLitForms(t *testing.T) {
	cases := []struct {
		in   ast.Literal
		want string
	}{
		{ast.IntLit(42), "42"},
		{ast.FloatLit(2.5), "2.5"},
		{ast.FloatLit(3.0), "3"},
		{ast.StrLit("hi"), "'hi'"},
		{ast.BoolLit(true), "true"},
		{ast.NullLit(), "null"},
		{ast.ParamLit(3), "$auto3"},
		{ast.NamedParamLit("who"), "$who"},
	}
	for _, c := range cases {
		if got := fmtLit(c.in); got != c.want {
			t.Fatalf("fmtLit(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFmtExprForms(t *testing.T) {
	v := &ast.Var{Name: "x"}
	prop := &ast.Prop{Var: "n", Key: "age"}
	cases := []struct {
		in   ast.Expr
		want string
	}{
		{&ast.Unary{Op: ast.Not, Expr: v}, "NOT x"},
		{&ast.Unary{Op: ast.Neg, Expr: v}, "-x"},
		{&ast.Binary{Op: ast.OpGte, LHS: prop, RHS: lit(ast.IntLit(1))}, "n.age >= 1"},
		{&ast.Binary{Op: ast.OpStartsWith, LHS: v, RHS: lit(ast.StrLit("a"))}, "x STARTS WITH 'a'"},
		{&ast.Binary{Op: ast.OpEndsWith, LHS: v, RHS: lit(ast.StrLit("a"))}, "x ENDS WITH 'a'"},
		{&ast.Binary{Op: ast.OpContains, LHS: v, RHS: lit(ast.StrLit("a"))}, "x CONTAINS 'a'"},
		{&ast.Binary{Op: ast.OpOr, LHS: v, RHS: v}, "x OR x"},
		{&ast.Binary{Op: ast.OpNeq, LHS: v, RHS: v}, "x <> x"},
		{&ast.Binary{Op: ast.OpLt, LHS: v, RHS: v}, "x < x"},
		{&ast.Binary{Op: ast.OpLte, LHS: v, RHS: v}, "x <= x"},
		{&ast.Binary{Op: ast.OpGt, LHS: v, RHS: v}, "x > x"},
		{&ast.Binary{Op: ast.OpAdd, LHS: v, RHS: v}, "x + x"},
		{&ast.Binary{Op: ast.OpSub, LHS: v, RHS: v}, "x - x"},
		{&ast.Binary{Op: ast.OpMul, LHS: v, RHS: v}, "x * x"},
		{&ast.Binary{Op: ast.OpDiv, LHS: v, RHS: v}, "x / x"},
		{&ast.Func{Name: "count", Star: true}, "count(*)"},
		{&ast.Func{Name: "collect", Distinct: true, Args: []ast.Expr{v}}, "collect(DISTINCT x)"},
		{&ast.ListExpr{Elems: []ast.Expr{lit(ast.IntLit(1)), lit(ast.IntLit(2))}}, "[1, 2]"},
		{&ast.In{Expr: v, List: &ast.Var{Name: "l"}}, "x IN l"},
		{&ast.IsNull{Expr: v}, "x IS NULL"},
		{&ast.IsNull{Expr: v, Negated: true}, "x IS NOT NULL"},
		{&ast.Case{}, "CASE…END"},
		{&ast.Cost{From: "a", To: "b"}, "cost(shortestPath((a)..(b)), …)"},
		{&ast.Exists{}, "EXISTS {…}"},
		{&ast.CountSub{}, "COUNT {…}"},
		{&ast.PatternComp{Proj: v}, "[(…) | x]"},
		{&ast.ListPred{Quant: ast.QuantSingle, Var: "y", List: v, Pred: v}, "single(y IN x WHERE …)"},
		{&ast.Reduce{Acc: "s", Init: lit(ast.IntLit(0)), Var: "y", List: v, Body: v}, "reduce(s = 0, y IN x | …)"},
		{&ast.ListComp{Var: "y", List: v}, "[y IN x …]"},
		{&ast.Index{Base: v, Idx: lit(ast.IntLit(0))}, "x[0]"},
		{&ast.Slice{Base: v, From: lit(ast.IntLit(1)), To: lit(ast.IntLit(2))}, "x[1..2]"},
		{&ast.Slice{Base: v}, "x[..]"},
		{&ast.PropOf{Base: v, Key: "k"}, "x.k"},
		{&ast.MapLit{Fields: []ast.MapField{{Key: "a", Val: v}}}, "{a: x}"},
		{&ast.MapProj{Var: "n", Entries: []ast.MapProjEntry{
			{Kind: ast.MapProjProp, Key: "a"},
			{Kind: ast.MapProjAll},
			{Kind: ast.MapProjField, Key: "b", Expr: v},
		}}, "n{.a, .*, b: x}"},
		{&ast.HasLabelExpr{Var: "n", Expr: &ast.LabelExpr{
			Kind: ast.LabelOr,
			L:    &ast.LabelExpr{Kind: ast.LabelAnd, L: &ast.LabelExpr{Kind: ast.LabelName, Name: "A"}, R: &ast.LabelExpr{Kind: ast.LabelName, Name: "B"}},
			R:    &ast.LabelExpr{Kind: ast.LabelNot, L: &ast.LabelExpr{Kind: ast.LabelName, Name: "C"}},
		}}, "n:((A&B)|!C)"},
	}
	for _, c := range cases {
		if got := fmtExpr(c.in); got != c.want {
			t.Fatalf("fmtExpr = %q, want %q", got, c.want)
		}
	}
	if got := fmtSort(ast.SortItem{Expr: v, Desc: true}); got != "x DESC" {
		t.Fatalf("fmtSort desc = %q", got)
	}
	if got := fmtSort(ast.SortItem{Expr: v}); got != "x" {
		t.Fatalf("fmtSort asc = %q", got)
	}
}

func TestCallLabels(t *testing.T) {
	cases := []struct {
		proc plan.CallProc
		want string
	}{
		{plan.CallProc{Kind: plan.ProcWcc, RelType: "KNOWS"}, "wcc('KNOWS')"},
		{plan.CallProc{Kind: plan.ProcFtsSearch, Label: "P", Field: "f", Query: "q"}, "fts.search('P', 'f', 'q')"},
		{plan.CallProc{Kind: plan.ProcGeoWithinRadius, Label: "P"}, "geo.withinRadius('P', …)"},
		{plan.CallProc{Kind: plan.ProcGeoWithinBBox, Label: "P"}, "geo.withinBBox('P', …)"},
		{plan.CallProc{Kind: plan.ProcBfs, Source: 3}, "algo.bfs(3, …)"},
		{plan.CallProc{Kind: plan.ProcPageRank}, "algo.pagerank(…)"},
		{plan.CallProc{Kind: plan.ProcWccAll}, "algo.wcc()"},
		{plan.CallProc{Kind: plan.ProcCdlp}, "algo.cdlp(…)"},
		{plan.CallProc{Kind: plan.ProcLcc}, "algo.lcc(…)"},
		{plan.CallProc{Kind: plan.ProcSssp, Source: 2}, "algo.sssp(2, …)"},
	}
	for _, c := range cases {
		if got := callLabel(&c.proc); got != c.want {
			t.Fatalf("callLabel = %q, want %q", got, c.want)
		}
	}
}

// TestProfileZipHandBuilt checks the row-count column placement with a
// hand-built profile over a synthetic single-stage plan.
func TestProfileZipHandBuilt(t *testing.T) {
	seg := &plan.Segment{
		RowWidth: 1,
		Slots:    map[string]int{"n": 0},
		Stages: []plan.Stage{&plan.MatchStage{
			Ops: []plan.BindOp{{
				Kind: plan.OpScan, Slot: 0,
				Source: plan.ScanSource{Kind: plan.ScanLabel, Label: "P"},
				Labels: []string{"P"},
			}},
			Where: &ast.Binary{Op: ast.OpGt, LHS: &ast.Prop{Var: "n", Key: "age"}, RHS: lit(ast.IntLit(1))},
		}},
		Proj: plan.ProjPlan{
			Returns: []plan.BoundReturn{{Expr: &ast.Var{Name: "n"}, Name: "n"}},
			Columns: []string{"n"},
		},
		PostWhere: &ast.Binary{Op: ast.OpLt, LHS: &ast.Var{Name: "n"}, RHS: lit(ast.IntLit(9))},
	}
	p := &plan.Plan{Branches: [][]*plan.Segment{{seg}}, Columns: []string{"n"}}
	pw := uint64(5)
	prof := &Profile{Segs: []SegProf{{
		Stages:        []StageProf{{Match: []uint64{12, 7}}},
		ProjRows:      7,
		PostWhereRows: &pw,
	}}}
	lines := Render(p, prof, 2*time.Millisecond, nil)
	text := strings.Join(lines, "\n")
	if lines[0] != "PROFILE" {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(text, "Planning: 2.000 ms") {
		t.Fatalf("planning line missing:\n%s", text)
	}
	find := func(needle, count string) {
		t.Helper()
		for _, l := range lines {
			if strings.Contains(l, needle) && strings.HasSuffix(strings.TrimRight(l, " "), count) {
				return
			}
		}
		t.Fatalf("no %q line ending in %s:\n%s", needle, count, text)
	}
	find("NodeScan", "12")
	find("Filter (n.age > 1)", "7")
	find("Project [n]", "7")
	find("Filter (n < 9)", "5")
}
