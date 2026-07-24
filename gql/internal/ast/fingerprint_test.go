// Fingerprint tests: structurally different queries must render
// differently (a collision would make the plan cache reuse the wrong
// plan); identical structure must render identically.
package ast_test

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
)

func fp(t *testing.T, src string) string {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return ast.Fingerprint(q)
}

func TestFingerprintDistinguishesStructure(t *testing.T) {
	// Every pair below is structurally distinct and must fingerprint
	// differently; the list sweeps all clause kinds and the expression
	// surface.
	queries := []string{
		"MATCH (p:Person) RETURN p.name AS n",
		"MATCH (p:Person) RETURN p.name AS m",
		"MATCH (p:People) RETURN p.name AS n",
		"MATCH (q:Person) RETURN q.name AS n",
		"MATCH (p:Person {age: 30}) RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.age >= 30 RETURN p.name AS n",
		"MATCH (p:Person) WHERE NOT p.age > 30 RETURN p.name AS n",
		"MATCH (p:Person) RETURN DISTINCT p.name AS n",
		"MATCH (p:Person) RETURN p.name AS n ORDER BY n",
		"MATCH (p:Person) RETURN p.name AS n ORDER BY n DESC",
		"MATCH (p:Person) RETURN p.name AS n LIMIT 2",
		"MATCH (p:Person) RETURN p.name AS n LIMIT 3",
		"MATCH (p:Person) RETURN p.name AS n OFFSET 1",
		"MATCH (p:Person) RETURN *",
		"OPTIONAL MATCH (p:Person) RETURN p.name AS n",
		"MATCH (a)-[:KNOWS]->(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]-(b) RETURN b.name AS n",
		"MATCH (a)<-[:KNOWS]-(b) RETURN b.name AS n",
		"MATCH (a)-[:LIKES]->(b) RETURN b.name AS n",
		"MATCH (a)-[r:KNOWS]->(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->{1,2}(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->{1,3}(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->*(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->+(b) RETURN b.name AS n",
		"MATCH (a:A&B) RETURN a AS a",
		"MATCH (a:A|B) RETURN a AS a",
		"MATCH (a:!A) RETURN a AS a",
		"MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		"MATCH (a), (b) MATCH p = ALL SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		"MATCH p = (a)-[:KNOWS]->(b) RETURN length(p) AS l",
		"CALL wcc('KNOWS') YIELD node, component RETURN component AS c",
		"CALL wcc('LIKES') YIELD node, component RETURN component AS c",
		"CALL algo.pagerank() YIELD node, value RETURN value AS v",
		"FOR x IN [1, 2] RETURN x AS x",
		"FOR y IN [1, 2] RETURN y AS y",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN c AS c",
		"MATCH (p:Person) CALL { MATCH (f:Person) RETURN count(f) AS c } RETURN c AS c",
		"MATCH (p:Person) RETURN count(*) AS n",
		"MATCH (p:Person) RETURN count(p) AS n",
		"MATCH (p:Person) RETURN count(DISTINCT p) AS n",
		"MATCH (p:Person) RETURN p.name AS n NEXT FILTER n = 'x' RETURN n",
		"MATCH (p:Person) LET a = p.age RETURN a AS a",
		"MATCH (p:Person) RETURN p.name AS n UNION MATCH (c:Co) RETURN c.name AS n",
		"MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (c:Co) RETURN c.name AS n",
		"MATCH (p) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q) } RETURN p AS p",
		"MATCH (p) WHERE COUNT { MATCH (p)-[:KNOWS]->(q) } > 1 RETURN p AS p",
		"MATCH (p) WHERE p.name IN ['a', 'b'] RETURN p AS p",
		"MATCH (p) WHERE p.name IS NULL RETURN p AS p",
		"MATCH (p) WHERE p.name IS NOT NULL RETURN p AS p",
		"MATCH (p) WHERE p:Extra RETURN p AS p",
		"MATCH (p) RETURN CASE WHEN p.a > 1 THEN 'x' ELSE 'y' END AS c",
		"MATCH (p) RETURN CASE p.a WHEN 1 THEN 'x' END AS c",
		"MATCH (p) RETURN {a: p.x, b: 2} AS m",
		"MATCH (p) RETURN [p.a, p.b][0] AS i",
		"MATCH (p) RETURN [p.a, p.b][0..1] AS s",
		"MATCH (p) RETURN all(x IN p.xs WHERE x > 0) AS q",
		"MATCH (p) RETURN any(x IN p.xs WHERE x > 0) AS q",
		"MATCH (p) RETURN -p.a + 2 * p.b AS e",
		"MATCH (p) RETURN p.name STARTS WITH 'A' AS e",
		"MATCH (p) RETURN p.name ENDS WITH 'A' AS e",
		"MATCH (p:Person {name: $who}) RETURN p.age AS a",
		"EXPLAIN MATCH (p:Person) RETURN p.name AS n",
		"PROFILE MATCH (p:Person) RETURN p.name AS n",
	}
	seen := map[string]string{}
	for _, q := range queries {
		f := fp(t, q)
		if prev, dup := seen[f]; dup {
			t.Fatalf("fingerprint collision:\n  %s\n  %s\n  -> %s", prev, q, f)
		}
		seen[f] = q
	}
}

// fpExpr wraps a single expression as a lone RETURN item and fingerprints the
// query, so a table of nodes can be compared for collisions directly. Direct
// construction reaches the engine-only expression nodes (Cost, Reduce,
// ListComp, PatternComp, MapProj) that have no GQL surface syntax and so are
// unreachable through the parser -- exactly where a missing fpExpr arm would be
// an invisible plan-cache collision.
func fpExpr(e ast.Expr) string {
	q := &ast.Query{Parts: []ast.QueryPart{{
		Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: e}}},
	}}}
	return ast.Fingerprint(q)
}

// fpClause wraps a single clause ahead of a `*` projection and fingerprints it.
func fpClause(c ast.Clause) string {
	q := &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{c},
		Ret:     ast.Projection{Star: true},
	}}}
	return ast.Fingerprint(q)
}

// minPattern is the smallest valid linear pattern: a single bound node.
func minPattern() *ast.Pattern { return &ast.Pattern{Start: ast.NodePat{Var: "x"}} }

// TestFingerprintCoversAllExprKinds constructs every Expr node kind -- including
// the engine-only nodes the parser cannot produce -- and asserts they all
// fingerprint distinctly. A collision here means two structurally different
// expressions would share one cached template plan.
func TestFingerprintCoversAllExprKinds(t *testing.T) {
	lbl := &ast.LabelExpr{Kind: ast.LabelName, Name: "L"}
	nodes := []ast.Expr{
		nil, // the explicit "_" arm (e.g. an absent Case operand rendered inline)
		&ast.Lit{Value: ast.IntLit(1)},
		&ast.Var{Name: "v"},
		&ast.Prop{Var: "v", Key: "k"},
		&ast.Unary{Op: ast.Not, Expr: &ast.Var{Name: "v"}},
		&ast.Binary{Op: ast.OpOr, LHS: &ast.Var{Name: "a"}, RHS: &ast.Var{Name: "b"}},
		&ast.Func{Name: "count", Star: true},
		&ast.Func{Name: "count", Distinct: true, Args: []ast.Expr{&ast.Var{Name: "v"}}},
		&ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}},
		&ast.In{Expr: &ast.Var{Name: "v"}, List: &ast.ListExpr{}},
		&ast.IsNull{Expr: &ast.Var{Name: "v"}},
		&ast.IsNull{Expr: &ast.Var{Name: "v"}, Negated: true},
		&ast.IsTruth{Expr: &ast.Var{Name: "v"}, Want: true},
		&ast.IsTruth{Expr: &ast.Var{Name: "v"}, Want: false},
		&ast.IsTruth{Expr: &ast.Var{Name: "v"}, Want: true, Negated: true},
		&ast.IsTyped{Expr: &ast.Var{Name: "v"}, Kind: "integer"},
		&ast.IsTyped{Expr: &ast.Var{Name: "v"}, Kind: "float"},
		&ast.IsTyped{Expr: &ast.Var{Name: "v"}, Kind: "integer", Negated: true},
		&ast.Case{
			Operand: &ast.Var{Name: "v"},
			Whens:   []ast.CaseWhen{{Cond: &ast.Lit{Value: ast.IntLit(1)}, Result: &ast.Lit{Value: ast.StrLit("x")}}},
			Else:    &ast.Lit{Value: ast.StrLit("y")},
		},
		&ast.Cost{From: "a", To: "b", Dir: ast.DirOut, Types: []string{"KNOWS"}, Weight: ast.CostSpec{Kind: ast.CostProperty, Prop: "w"}},
		&ast.Exists{Pattern: minPattern()},
		&ast.Exists{Pattern: minPattern(), Where: &ast.Var{Name: "v"}},
		&ast.CountSub{Pattern: minPattern()},
		&ast.ListPred{Quant: ast.QuantAll, Var: "x", List: &ast.Var{Name: "xs"}, Pred: &ast.Var{Name: "x"}},
		&ast.ListPred{Quant: ast.QuantAny, Var: "x", List: &ast.Var{Name: "xs"}, Pred: &ast.Var{Name: "x"}},
		&ast.Reduce{Acc: "s", Init: &ast.Lit{Value: ast.IntLit(0)}, Var: "x", List: &ast.Var{Name: "xs"}, Body: &ast.Var{Name: "x"}},
		&ast.ListComp{Var: "x", List: &ast.Var{Name: "xs"}, Filter: &ast.Var{Name: "x"}, Map: &ast.Var{Name: "x"}},
		&ast.PatternComp{Pattern: minPattern(), Proj: &ast.Var{Name: "x"}},
		&ast.Index{Base: &ast.Var{Name: "v"}, Idx: &ast.Lit{Value: ast.IntLit(0)}},
		&ast.Slice{Base: &ast.Var{Name: "v"}, From: &ast.Lit{Value: ast.IntLit(0)}, To: &ast.Lit{Value: ast.IntLit(1)}},
		&ast.Slice{Base: &ast.Var{Name: "v"}}, // both bounds nil -> the "_" arms
		&ast.PropOf{Base: &ast.Var{Name: "v"}, Key: "k"},
		&ast.MapProj{Var: "v", Entries: []ast.MapProjEntry{
			{Kind: ast.MapProjProp, Key: "k"},
			{Kind: ast.MapProjField, Key: "a", Expr: &ast.Var{Name: "x"}},
			{Kind: ast.MapProjAll},
		}},
		&ast.MapLit{Fields: []ast.MapField{{Key: "a", Val: &ast.Lit{Value: ast.IntLit(1)}}}},
		&ast.HasLabelExpr{Var: "v", Expr: lbl},
	}
	seen := map[string]int{}
	for i, e := range nodes {
		f := fpExpr(e)
		if prev, dup := seen[f]; dup {
			t.Fatalf("expr fingerprint collision: nodes[%d] and nodes[%d] both -> %q", prev, i, f)
		}
		seen[f] = i
		if fpExpr(e) != f {
			t.Fatalf("nodes[%d] fingerprint not stable", i)
		}
	}
}

// TestFingerprintCoversAllLiteralKinds asserts each Literal kind fingerprints
// distinctly, including the param forms and both boolean values.
func TestFingerprintCoversAllLiteralKinds(t *testing.T) {
	lits := []ast.Literal{
		ast.IntLit(7),
		ast.FloatLit(1.5),
		ast.StrLit("s"),
		ast.BoolLit(true),
		ast.BoolLit(false),
		ast.NullLit(),
		ast.ParamLit(3),
		ast.NamedParamLit("who"),
	}
	seen := map[string]int{}
	for i, l := range lits {
		f := fpExpr(&ast.Lit{Value: l})
		if prev, dup := seen[f]; dup {
			t.Fatalf("literal fingerprint collision: lits[%d] and lits[%d] both -> %q", prev, i, f)
		}
		seen[f] = i
	}
}

// TestFingerprintCoversEngineClauses fingerprints the clause kinds and the
// weighted/optional variants, asserting each is distinct -- the fpClause arms a
// parse-only test reaches unevenly.
func TestFingerprintCoversEngineClauses(t *testing.T) {
	clauses := []ast.Clause{
		&ast.Match{Patterns: []ast.Pattern{*minPattern()}},
		&ast.Match{Patterns: []ast.Pattern{*minPattern()}, Optional: true},
		&ast.Match{Patterns: []ast.Pattern{*minPattern()}, Acyclic: true},
		&ast.Match{Patterns: []ast.Pattern{*minPattern()}, Repeatable: true},
		&ast.With{Proj: ast.Projection{Star: true}, Where: &ast.Var{Name: "v"}},
		&ast.ShortestPath{PathVar: "p", Pattern: *minPattern()},
		&ast.ShortestPath{PathVar: "p", Pattern: *minPattern(), All: true, Optional: true},
		&ast.ShortestPath{PathVar: "p", Pattern: *minPattern(), Weight: &ast.CostSpec{Kind: ast.CostConstant, Const: 2}},
		&ast.CallProc{Proc: "wcc", Args: []ast.Expr{&ast.Lit{Value: ast.StrLit("KNOWS")}}, Yields: []ast.YieldItem{{Field: "node", Alias: "n"}}},
		&ast.PathBind{PathVar: "p", Pattern: *minPattern()},
		&ast.PathBind{PathVar: "p", Pattern: *minPattern(), Acyclic: true, Optional: true},
		&ast.Unwind{Var: "x", Expr: &ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}}},
		&ast.CallSubquery{Imports: []string{"p"}, Query: ast.Query{Parts: []ast.QueryPart{{Ret: ast.Projection{Star: true}}}}},
	}
	seen := map[string]int{}
	for i, c := range clauses {
		f := fpClause(c)
		if prev, dup := seen[f]; dup {
			t.Fatalf("clause fingerprint collision: clauses[%d] and clauses[%d] both -> %q", prev, i, f)
		}
		seen[f] = i
	}
}

// TestFingerprintCoversRelPatternDetails exercises fpRelPat's property and
// variable-length arms: literal props, expression props, and the three
// partial-bound quantifier shapes must each shift the fingerprint.
func TestFingerprintCoversRelPatternDetails(t *testing.T) {
	two, three := uint64(2), uint64(3)
	rels := []ast.RelPat{
		{Var: "r", Dir: ast.DirOut, Types: []string{"KNOWS"}},
		{Dir: ast.DirOut, Props: []ast.PropEntry{{Key: "since", Val: ast.IntLit(2020)}}},
		{Dir: ast.DirOut, PropExprs: []ast.PropExprEntry{{Key: "w", Val: &ast.Var{Name: "x"}}}},
		{Dir: ast.DirOut, Length: &ast.VarLength{Min: &two, Max: &three}},
		{Dir: ast.DirOut, Length: &ast.VarLength{Min: &two}},   // {2,}
		{Dir: ast.DirOut, Length: &ast.VarLength{Max: &three}}, // {,3}
		{Dir: ast.DirOut, Length: &ast.VarLength{}},            // * (both nil)
	}
	seen := map[string]int{}
	for i, r := range rels {
		pat := ast.Pattern{Start: ast.NodePat{Var: "a"}, Hops: []ast.PatternHop{{Rel: r, Node: ast.NodePat{Var: "b"}}}}
		f := fpClause(&ast.Match{Patterns: []ast.Pattern{pat}})
		if prev, dup := seen[f]; dup {
			t.Fatalf("rel-pattern fingerprint collision: rels[%d] and rels[%d] both -> %q", prev, i, f)
		}
		seen[f] = i
	}
}

func TestFingerprintStableAcrossParses(t *testing.T) {
	for _, q := range []string{
		"MATCH (p:Person {name: 'Alice'})-[:KNOWS]->{1,2}(f) WHERE f.age > 30 RETURN DISTINCT f.name AS n ORDER BY n LIMIT 5",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN c AS c",
	} {
		// Whitespace-insensitive: the fingerprint reflects structure, not
		// the raw text.
		spaced := "  " + q + "  "
		if fp(t, q) != fp(t, spaced) {
			t.Fatalf("whitespace changed the fingerprint: %s", q)
		}
		if fp(t, q) != fp(t, q) {
			t.Fatalf("unstable fingerprint: %s", q)
		}
	}
}
