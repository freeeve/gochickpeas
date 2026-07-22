// Statement-level parser tests, ported from the Rust engine's
// tests/parse.rs with each query translated to the GQL surface (WITH ->
// RETURN...NEXT / LET / FILTER, UNWIND -> FOR, *1..3 -> {1,3},
// shortestPath -> ANY SHORTEST), asserting the same AST shapes.
package parser

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// mustParse parses or fails the test.
func mustParse(t *testing.T, src string) *ast.Query {
	t.Helper()
	q, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return q
}

// mustErr asserts a parse error mentioning want.
func mustErr(t *testing.T, src, want string) {
	t.Helper()
	_, err := Parse(src)
	if err == nil {
		t.Fatalf("parse %q: expected error containing %q", src, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("parse %q: error %q does not mention %q", src, err, want)
	}
}

// oneMatch returns the first clause as a Match's first pattern + where.
func oneMatch(t *testing.T, q *ast.Query) (*ast.Pattern, ast.Expr) {
	t.Helper()
	m, ok := q.Parts[0].Clauses[0].(*ast.Match)
	if !ok {
		t.Fatalf("expected a MATCH clause, got %T", q.Parts[0].Clauses[0])
	}
	return &m.Patterns[0], m.Where
}

func TestWorkedExample(t *testing.T) {
	q := mustParse(t,
		"MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 30 "+
			"RETURN f.name AS name, count(*) AS c ORDER BY c DESC LIMIT 10")
	pat, where := oneMatch(t, q)
	if pat.Start.Var != "p" || len(pat.Start.Labels) != 1 || pat.Start.Labels[0] != "Person" {
		t.Fatalf("start = %+v", pat.Start)
	}
	if len(pat.Hops) != 1 {
		t.Fatalf("hops = %d", len(pat.Hops))
	}
	hop := pat.Hops[0]
	if hop.Rel.Dir != ast.DirOut || len(hop.Rel.Types) != 1 || hop.Rel.Types[0] != "KNOWS" {
		t.Fatalf("rel = %+v", hop.Rel)
	}
	if hop.Node.Var != "f" || hop.Node.Labels[0] != "Person" {
		t.Fatalf("end = %+v", hop.Node)
	}
	bin, ok := where.(*ast.Binary)
	if !ok || bin.Op != ast.OpGt {
		t.Fatalf("where = %#v", where)
	}
	if p, ok := bin.LHS.(*ast.Prop); !ok || p.Var != "p" || p.Key != "age" {
		t.Fatalf("lhs = %#v", bin.LHS)
	}
	if l, ok := bin.RHS.(*ast.Lit); !ok || l.Value != ast.IntLit(30) {
		t.Fatalf("rhs = %#v", bin.RHS)
	}
	ret := q.Parts[0].Ret
	if ret.Distinct || len(ret.Items) != 2 {
		t.Fatalf("ret = %+v", ret)
	}
	if ret.Items[0].Alias != "name" {
		t.Fatalf("item0 = %+v", ret.Items[0])
	}
	f, ok := ret.Items[1].Expr.(*ast.Func)
	if !ok || f.Name != "count" || !f.Star || f.Distinct {
		t.Fatalf("item1 = %#v", ret.Items[1].Expr)
	}
	if ret.Items[1].Alias != "c" {
		t.Fatalf("item1 alias = %q", ret.Items[1].Alias)
	}
	if len(ret.OrderBy) != 1 || !ret.OrderBy[0].Desc {
		t.Fatalf("order = %+v", ret.OrderBy)
	}
	if v, ok := ret.OrderBy[0].Expr.(*ast.Var); !ok || v.Name != "c" {
		t.Fatalf("order expr = %#v", ret.OrderBy[0].Expr)
	}
	if ret.Limit == nil || *ret.Limit != 10 || ret.Skip != nil {
		t.Fatalf("limit/skip = %v %v", ret.Limit, ret.Skip)
	}
}

func TestRelationshipDirections(t *testing.T) {
	for _, tc := range []struct {
		src string
		dir ast.Dir
	}{
		{"MATCH (a)-[:R]->(b) RETURN a", ast.DirOut},
		{"MATCH (a)<-[:R]-(b) RETURN a", ast.DirIn},
		{"MATCH (a)-[:R]-(b) RETURN a", ast.DirBoth},
		{"MATCH (a)-->(b) RETURN a", ast.DirOut},
		{"MATCH (a)<--(b) RETURN a", ast.DirIn},
		{"MATCH (a)--(b) RETURN a", ast.DirBoth},
	} {
		pat, _ := oneMatch(t, mustParse(t, tc.src))
		if pat.Hops[0].Rel.Dir != tc.dir {
			t.Fatalf("%s: dir = %v, want %v", tc.src, pat.Hops[0].Rel.Dir, tc.dir)
		}
	}
}

func TestRelTypeAlternationAndVar(t *testing.T) {
	pat, _ := oneMatch(t, mustParse(t, "MATCH (a)-[r:KNOWS|LIKES]->(b) RETURN a"))
	rel := pat.Hops[0].Rel
	if rel.Var != "r" || len(rel.Types) != 2 || rel.Types[0] != "KNOWS" || rel.Types[1] != "LIKES" {
		t.Fatalf("rel = %+v", rel)
	}
}

func TestInlinePropertyMap(t *testing.T) {
	pat, _ := oneMatch(t, mustParse(t, "MATCH (p:Person {name: 'Alice', age: 30}) RETURN p"))
	props := pat.Start.Props
	if len(props) != 2 || props[0].Key != "name" || props[0].Val != ast.StrLit("Alice") ||
		props[1].Key != "age" || props[1].Val != ast.IntLit(30) {
		t.Fatalf("props = %+v", props)
	}
}

func TestInlinePropertyExprGoesToPropExprs(t *testing.T) {
	pat, _ := oneMatch(t, mustParse(t, "MATCH (p:Person {name: tagVar}) RETURN p"))
	if len(pat.Start.Props) != 0 || len(pat.Start.PropExprs) != 1 || pat.Start.PropExprs[0].Key != "name" {
		t.Fatalf("start = %+v", pat.Start)
	}
	// A $param stays on the literal fast path.
	pat2, _ := oneMatch(t, mustParse(t, "MATCH (p:Person {name: $who}) RETURN p"))
	if len(pat2.Start.Props) != 1 || pat2.Start.Props[0].Val != ast.NamedParamLit("who") {
		t.Fatalf("param prop = %+v", pat2.Start)
	}
}

func TestQuantifiers(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	for _, tc := range []struct {
		src      string
		min, max *uint64
	}{
		{"MATCH (a)-[:KNOWS]->{1,2}(b) RETURN b", u(1), u(2)},
		{"MATCH (a)-[:KNOWS]->{3}(b) RETURN b", u(3), u(3)},
		{"MATCH (a)-[:KNOWS]->{,2}(b) RETURN b", nil, u(2)},
		{"MATCH (a)-[:KNOWS]->{2,}(b) RETURN b", u(2), nil},
		{"MATCH (a)-[:KNOWS]->*(b) RETURN b", nil, nil},
		{"MATCH (a)-[:KNOWS]->+(b) RETURN b", u(1), nil},
		{"MATCH (a)-[:KNOWS]-{1,4}(b) RETURN b", u(1), u(4)},
	} {
		pat, _ := oneMatch(t, mustParse(t, tc.src))
		vl := pat.Hops[0].Rel.Length
		if vl == nil {
			t.Fatalf("%s: no quantifier", tc.src)
		}
		eq := func(a, b *uint64) bool {
			if (a == nil) != (b == nil) {
				return false
			}
			return a == nil || *a == *b
		}
		if !eq(vl.Min, tc.min) || !eq(vl.Max, tc.max) {
			t.Fatalf("%s: length = %v/%v", tc.src, vl.Min, vl.Max)
		}
	}
	// A single fixed hop has no quantifier.
	pat, _ := oneMatch(t, mustParse(t, "MATCH (a)-[:KNOWS]->(b) RETURN b"))
	if pat.Hops[0].Rel.Length != nil {
		t.Fatal("fixed hop must have nil Length")
	}
	// The Cypher in-bracket spelling is rejected with a pointer to GQL.
	mustErr(t, "MATCH (a)-[:KNOWS*1..2]->(b) RETURN b", "quantify after the arrow")
}

func TestOptionalMatch(t *testing.T) {
	q := mustParse(t, "MATCH (p:Person) OPTIONAL MATCH (p)-[:WORKS_AT]->(c) RETURN p.name AS n, c.name AS co")
	if m := q.Parts[0].Clauses[0].(*ast.Match); m.Optional {
		t.Fatal("first match is not optional")
	}
	if m := q.Parts[0].Clauses[1].(*ast.Match); !m.Optional {
		t.Fatal("second match is optional")
	}
}

func TestCommaSeparatedPatterns(t *testing.T) {
	q := mustParse(t, "MATCH (a)-[:KNOWS]->(b), (b)-[:KNOWS]->(c) RETURN c")
	m := q.Parts[0].Clauses[0].(*ast.Match)
	if len(m.Patterns) != 2 || m.Patterns[0].Start.Var != "a" || m.Patterns[1].Start.Var != "b" {
		t.Fatalf("patterns = %+v", m.Patterns)
	}
}

func TestPathSearchClauses(t *testing.T) {
	q := mustParse(t, "MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:KNOWS]-{1,4}(b) RETURN length(p)")
	sp, ok := q.Parts[0].Clauses[1].(*ast.ShortestPath)
	if !ok || sp.PathVar != "p" || sp.All || sp.Weight != nil {
		t.Fatalf("clause = %#v", q.Parts[0].Clauses[1])
	}
	vl := sp.Pattern.Hops[0].Rel.Length
	if vl == nil || *vl.Min != 1 || *vl.Max != 4 {
		t.Fatalf("length = %+v", vl)
	}
	q2 := mustParse(t, "MATCH (a), (b) MATCH p = ALL SHORTEST (a)-[:KNOWS]-{,4}(b) RETURN length(p)")
	sp2 := q2.Parts[0].Clauses[1].(*ast.ShortestPath)
	if !sp2.All {
		t.Fatal("ALL SHORTEST sets All")
	}
	if sp2.Pattern.Hops[0].Rel.Length.Min != nil || *sp2.Pattern.Hops[0].Rel.Length.Max != 4 {
		t.Fatalf("length = %+v", sp2.Pattern.Hops[0].Rel.Length)
	}
	mustErr(t, "MATCH p = SHORTEST 2 (a)-[:R]->(b) RETURN p", "ANY SHORTEST or ALL SHORTEST")
	// The Cypher function spellings point at the GQL prefix forms.
	mustErr(t, "MATCH (a), (b) MATCH p = shortestPath((a)-[:KNOWS]-(b)) RETURN p", "ANY SHORTEST")
	mustErr(t, "MATCH (a), (b) RETURN cost(shortestPath((a)-[:E]->(b)), 'w')", "not in the GQL subset")
}

func TestPathSearchCost(t *testing.T) {
	// A numeric literal lowers to a constant weight (int and float).
	q := mustParse(t, "MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) COST 2 RETURN length(p)")
	sp := q.Parts[0].Clauses[1].(*ast.ShortestPath)
	if sp.Weight == nil || sp.Weight.Kind != ast.CostConstant || sp.Weight.Const != 2 {
		t.Fatalf("weight = %+v", sp.Weight)
	}
	q = mustParse(t, "MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) COST 2.5 RETURN length(p)")
	if w := q.Parts[0].Clauses[1].(*ast.ShortestPath).Weight; w.Kind != ast.CostConstant || w.Const != 2.5 {
		t.Fatalf("weight = %+v", w)
	}
	// Anything else stays an expression (the planner narrows rel.prop).
	q = mustParse(t, "MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST r.w RETURN length(p)")
	w := q.Parts[0].Clauses[1].(*ast.ShortestPath).Weight
	if w.Kind != ast.CostExpr {
		t.Fatalf("weight = %+v", w)
	}
	if pr, ok := w.Expr.(*ast.Prop); !ok || pr.Var != "r" || pr.Key != "w" {
		t.Fatalf("weight expr = %#v", w.Expr)
	}
	// COST composes with a trailing WHERE.
	q = mustParse(t, "MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST r.w WHERE all(x IN rels(p) WHERE x.w > 0) RETURN length(p)")
	sp = q.Parts[0].Clauses[1].(*ast.ShortestPath)
	if sp.Weight == nil || sp.Where == nil {
		t.Fatalf("weight = %+v, where = %v", sp.Weight, sp.Where)
	}
	// COST is a path-search clause, not a path-bind one.
	mustErr(t, "MATCH p = (a)-[r:R]->(b) COST r.w RETURN p", "COST applies only to a path search")
}

func TestPathBind(t *testing.T) {
	q := mustParse(t, "MATCH p = (a)-[:KNOWS]->(b) RETURN p")
	pb, ok := q.Parts[0].Clauses[0].(*ast.PathBind)
	if !ok || pb.PathVar != "p" || len(pb.Pattern.Hops) != 1 {
		t.Fatalf("clause = %#v", q.Parts[0].Clauses[0])
	}
}

func TestNextLowersToWith(t *testing.T) {
	q := mustParse(t,
		"MATCH (p:Person)-[:KNOWS]->(f) RETURN p, count(*) AS c "+
			"NEXT FILTER c > 1 RETURN p.name AS name ORDER BY c DESC")
	if len(q.Parts) != 1 {
		t.Fatalf("parts = %d", len(q.Parts))
	}
	cl := q.Parts[0].Clauses
	if len(cl) != 3 {
		t.Fatalf("clauses = %d", len(cl))
	}
	w, ok := cl[1].(*ast.With)
	if !ok || len(w.Proj.Items) != 2 || w.Proj.Items[1].Alias != "c" || w.Where != nil {
		t.Fatalf("boundary = %#v", cl[1])
	}
	f, ok := cl[2].(*ast.With)
	if !ok || !f.Proj.Star || f.Where == nil {
		t.Fatalf("filter = %#v", cl[2])
	}
	bin := f.Where.(*ast.Binary)
	if bin.Op != ast.OpGt {
		t.Fatalf("filter where = %#v", f.Where)
	}
	if q.Parts[0].Ret.Items[0].Alias != "name" {
		t.Fatalf("ret = %+v", q.Parts[0].Ret)
	}
}

func TestLetLowersToStarProjection(t *testing.T) {
	q := mustParse(t, "MATCH (p:Person) LET a = p.age + 1, b = p.name RETURN a, b")
	w, ok := q.Parts[0].Clauses[1].(*ast.With)
	if !ok || !w.Proj.Star || len(w.Proj.Items) != 2 {
		t.Fatalf("let = %#v", q.Parts[0].Clauses[1])
	}
	if w.Proj.Items[0].Alias != "a" || w.Proj.Items[1].Alias != "b" {
		t.Fatalf("aliases = %+v", w.Proj.Items)
	}
}

func TestForLowersToUnwind(t *testing.T) {
	q := mustParse(t, "FOR x IN [1, 2, 3] RETURN x")
	u, ok := q.Parts[0].Clauses[0].(*ast.Unwind)
	if !ok || u.Var != "x" {
		t.Fatalf("for = %#v", q.Parts[0].Clauses[0])
	}
	if _, ok := u.Expr.(*ast.ListExpr); !ok {
		t.Fatalf("list = %#v", u.Expr)
	}
}

func TestCypherSpellingsRejected(t *testing.T) {
	mustErr(t, "MATCH (p) WITH p RETURN p", "RETURN ... NEXT")
	mustErr(t, "UNWIND [1] AS x RETURN x", "FOR x IN")
}

func TestWriteStatementsRejected(t *testing.T) {
	for _, src := range []string{
		"INSERT (n:Person) RETURN n",
		"MATCH (n) SET n.x = 1 RETURN n",
		"MATCH (n) DELETE n RETURN 1",
		"CREATE GRAPH g RETURN 1",
		"MATCH (n) REMOVE n.x RETURN n",
	} {
		mustErr(t, src, "read-only")
	}
}

func TestExplainProfilePrefix(t *testing.T) {
	if q := mustParse(t, "EXPLAIN MATCH (a) RETURN a"); q.Mode != ast.Explain {
		t.Fatal("explain mode")
	}
	if q := mustParse(t, "PROFILE MATCH (a) RETURN a"); q.Mode != ast.Profile {
		t.Fatal("profile mode")
	}
	if q := mustParse(t, "MATCH (a) RETURN a"); q.Mode != ast.Run {
		t.Fatal("run mode")
	}
}

func TestOffsetAndSkipBothAccepted(t *testing.T) {
	for _, src := range []string{
		"MATCH (a) RETURN a ORDER BY a.x OFFSET 5 LIMIT 2",
		"MATCH (a) RETURN a ORDER BY a.x SKIP 5 LIMIT 2",
	} {
		ret := mustParse(t, src).Parts[0].Ret
		if ret.Skip == nil || *ret.Skip != 5 || ret.Limit == nil || *ret.Limit != 2 {
			t.Fatalf("%s: skip/limit = %v/%v", src, ret.Skip, ret.Limit)
		}
	}
}

func TestUnionParsesIntoBranches(t *testing.T) {
	q := mustParse(t, "MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (c:Company) RETURN c.name AS n")
	if len(q.Parts) != 2 || len(q.Union) != 1 || q.Union[0] != ast.UnionAll {
		t.Fatalf("parts/union = %d/%v", len(q.Parts), q.Union)
	}
	q2 := mustParse(t, "FOR x IN [1] RETURN x AS n UNION FOR x IN [2] RETURN x AS n")
	if q2.Union[0] != ast.UnionDistinct {
		t.Fatal("bare UNION is distinct")
	}
	q3 := mustParse(t,
		"FOR x IN [1] RETURN x AS n UNION FOR x IN [2] RETURN x AS n UNION ALL FOR x IN [3] RETURN x AS n")
	if len(q3.Parts) != 3 || q3.Union[0] != ast.UnionDistinct || q3.Union[1] != ast.UnionAll {
		t.Fatalf("chain = %v", q3.Union)
	}
	q4 := mustParse(t, "MATCH (a:Person) RETURN a.name AS n")
	if len(q4.Parts) != 1 || len(q4.Union) != 0 {
		t.Fatal("single part")
	}
}

func TestRejectsTrailingGarbageAndMissingReturn(t *testing.T) {
	mustErr(t, "MATCH (a) RETURN a BOGUS", "trailing")
	mustErr(t, "MATCH (a)", "expected a statement")
}

func TestLabelExpressions(t *testing.T) {
	pat, _ := oneMatch(t, mustParse(t, "MATCH (n:Dog|Cat) RETURN n"))
	if len(pat.Start.Labels) != 0 || pat.Start.LabelExpr == nil || pat.Start.LabelExpr.Kind != ast.LabelOr {
		t.Fatalf("or = %+v", pat.Start)
	}
	if pat.Start.LabelExpr.L.Name != "Dog" || pat.Start.LabelExpr.R.Name != "Cat" {
		t.Fatalf("or leaves = %+v", pat.Start.LabelExpr)
	}
	for _, src := range []string{"MATCH (n:Dog&Cat) RETURN n", "MATCH (n:Dog:Cat) RETURN n"} {
		p, _ := oneMatch(t, mustParse(t, src))
		if len(p.Start.Labels) != 2 || p.Start.Labels[0] != "Dog" || p.Start.Labels[1] != "Cat" || p.Start.LabelExpr != nil {
			t.Fatalf("%s: conjunction = %+v", src, p.Start)
		}
	}
	p4, _ := oneMatch(t, mustParse(t, "MATCH (n:!Dog) RETURN n"))
	if p4.Start.LabelExpr == nil || p4.Start.LabelExpr.Kind != ast.LabelNot {
		t.Fatalf("not = %+v", p4.Start)
	}
	p5, _ := oneMatch(t, mustParse(t, "MATCH (n:Dog) RETURN n"))
	if len(p5.Start.Labels) != 1 || p5.Start.LabelExpr != nil {
		t.Fatalf("plain = %+v", p5.Start)
	}
	// Parenthesized grouping under negation.
	p6, _ := oneMatch(t, mustParse(t, "MATCH (n:!(Dog|Cat)&Pet) RETURN n"))
	if p6.Start.LabelExpr == nil || p6.Start.LabelExpr.Kind != ast.LabelAnd {
		t.Fatalf("grouped = %+v", p6.Start)
	}
}

// TestCorpusSurface covers the constructs the LDBC GQL manifest added to
// the dialect (task 012 deliverable 3): CAST, IS [NOT] LABELED, path-mode
// prefixes, the standalone ORDER BY statement, and MATCH-less
// EXISTS/COUNT subquery bodies.
func TestCorpusSurface(t *testing.T) {
	// CAST lowers to the matching conversion function.
	e := retExpr(t, "MATCH (a) RETURN CAST(a.x AS FLOAT) AS f")
	f, ok := e.(*ast.Func)
	if !ok || f.Name != "tofloat" || len(f.Args) != 1 {
		t.Fatalf("CAST float = %#v", e)
	}
	if g := retExpr(t, "MATCH (a) RETURN CAST(a.x AS INTEGER) AS i").(*ast.Func); g.Name != "tointeger" {
		t.Fatalf("CAST integer = %+v", g)
	}
	mustErr(t, "MATCH (a) RETURN CAST(a.x AS BLOB) AS b", "CAST target")

	// IS LABELED desugars to the label predicate; IS NOT LABELED negates.
	le := retExpr(t, "MATCH (m) RETURN m IS LABELED Comment AS c")
	if h, ok := le.(*ast.HasLabelExpr); !ok || h.Var != "m" {
		t.Fatalf("IS LABELED = %#v", le)
	}
	ne := retExpr(t, "MATCH (m) RETURN m IS NOT LABELED Comment AS c")
	if u, ok := ne.(*ast.Unary); !ok || u.Op != ast.Not {
		t.Fatalf("IS NOT LABELED = %#v", ne)
	}
	mustErr(t, "MATCH (m) RETURN m IS TRUTHY AS c", "after IS")

	// Path modes: TRAIL is a no-op, ACYCLIC sets the flag, WALK/SIMPLE
	// are rejected; both positions parse.
	q := mustParse(t, "MATCH TRAIL (a)-[:R]-{1,2}(b) RETURN b")
	if m := q.Parts[0].Clauses[0].(*ast.Match); m.Acyclic {
		t.Fatalf("TRAIL should not set Acyclic: %+v", m)
	}
	q2 := mustParse(t, "MATCH p = ACYCLIC (a)-[:R]->{1,3}(b) RETURN b")
	if pb := q2.Parts[0].Clauses[0].(*ast.PathBind); !pb.Acyclic || pb.PathVar != "p" {
		t.Fatalf("ACYCLIC path bind = %+v", pb)
	}
	mustErr(t, "MATCH WALK (a)-[:R]-{1,2}(b) RETURN b", "WALK path mode")
	mustErr(t, "MATCH SIMPLE (a)-[:R]-{1,2}(b) RETURN b", "SIMPLE path mode")

	// Standalone ORDER BY [LIMIT] lowers to a star projection boundary.
	q3 := mustParse(t, "MATCH (a:L) RETURN a NEXT ORDER BY a.x DESC LIMIT 1 MATCH (a)-[:R]->(b) RETURN b")
	w, ok := q3.Parts[0].Clauses[2].(*ast.With)
	if !ok || !w.Proj.Star || len(w.Proj.OrderBy) != 1 || !w.Proj.OrderBy[0].Desc || w.Proj.Limit == nil {
		t.Fatalf("standalone ORDER BY = %#v", q3.Parts[0].Clauses[2])
	}

	// EXISTS / COUNT subqueries accept a bare pattern body.
	be := retExpr(t, "MATCH (f) RETURN NOT EXISTS { (f)-[:R]->(:City) } AS c")
	if u, ok := be.(*ast.Unary); !ok || u.Op != ast.Not {
		t.Fatalf("bare EXISTS = %#v", be)
	}
	ce := retExpr(t, "MATCH (f) RETURN COUNT { (f)-[:R]->(:City) } AS c")
	if _, ok := ce.(*ast.CountSub); !ok {
		t.Fatalf("bare COUNT = %#v", ce)
	}

	// zoned_datetime / collect_list spellings parse as calls.
	q4 := mustParse(t, "MATCH (m) FILTER m.d < zoned_datetime('2011-12-01') RETURN collect_list(DISTINCT m.x) AS xs")
	if agg := q4.Parts[0].Ret.Items[0].Expr.(*ast.Func); agg.Name != "collect_list" || !agg.Distinct {
		t.Fatalf("collect_list = %+v", agg)
	}

	// A bare NEXT between statements is a no-op separator (the binding
	// table flows forward); RETURN ... NEXT stays the projecting boundary.
	q5 := mustParse(t, "MATCH (a) LET x = a.v NEXT MATCH (a)-[:R]->(b) RETURN b")
	if n := len(q5.Parts[0].Clauses); n != 3 {
		t.Fatalf("bare NEXT should not add a clause: %d clauses", n)
	}
}
