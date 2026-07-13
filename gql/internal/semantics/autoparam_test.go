// Autoparam tables ported from the Rust autoparam.rs tests, queries
// translated to the GQL surface (FILTER for WITH..WHERE, FOR for UNWIND,
// {1,3} quantifiers); the lift set and slot order must match exactly.
package semantics

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// lifted parses q, auto-parameterizes it, and returns the lifted values in
// slot order as compact tags (i/f/s/b/null), mirroring the Rust test
// helper.
func lifted(t *testing.T, q string) []string {
	t.Helper()
	query := parse(t, q)
	return tags(AutoParameterize(query))
}

func tags(vals []value.Value) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		switch v.Kind() {
		case value.KindInt:
			n, _ := v.AsInt()
			out[i] = fmt.Sprintf("i%d", n)
		case value.KindFloat:
			f, _ := v.AsFloat()
			out[i] = fmt.Sprintf("f%v", f)
		case value.KindStr:
			s, _ := v.AsStr()
			out[i] = "s" + s
		case value.KindBool:
			b, _ := v.AsBool()
			out[i] = fmt.Sprintf("b%v", b)
		case value.KindNull:
			out[i] = "null"
		default:
			out[i] = "other"
		}
	}
	return out
}

func wantLifted(t *testing.T, q string, want ...string) {
	t.Helper()
	got := lifted(t, q)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("%q: lifted %v, want none", q, got)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q: lifted %v, want %v", q, got, want)
	}
}

func TestLiftsPatternRelAndWhereBoundsInOrder(t *testing.T) {
	// Pattern start props, rel props, end-node props, then WHERE
	// comparison bounds (both operand orders and STARTS WITH) -- in a
	// fixed left-to-right slot order.
	wantLifted(t,
		"MATCH (p:Person {id: 669, name: 'India'})-[r:KNOWS {since: 2020}]->(f {age: 30}) "+
			"WHERE p.day < 5 AND 10 > f.score AND p.name STARTS WITH 'In' RETURN p",
		"i669", "sIndia", "i2020", "i30", "i5", "i10", "sIn")
}

func TestNullInlinePropIsStructuralNotLifted(t *testing.T) {
	// {x: null} is a structural match, left in place; the FILTER bound
	// lifts (GQL surface for Cypher's WITH a WHERE a.y = 42).
	wantLifted(t, "MATCH (a {x: null}) FILTER a.y = 42 RETURN a", "i42")
}

func TestLiftsInsideExistsSubquery(t *testing.T) {
	wantLifted(t,
		"MATCH (a) WHERE EXISTS { MATCH (a)-[:R]->(b {tag: 'z'}) WHERE b.n > 3 } RETURN a",
		"sz", "i3")
}

func TestLiftsInsideCallSubqueryBody(t *testing.T) {
	wantLifted(t, "MATCH (x) CALL { MATCH (m {v: 5}) RETURN m } RETURN x", "i5")
}

func TestLiftsVarLengthPathBindStartProps(t *testing.T) {
	// The path-bind pattern lifts its inline props; the quantifier bounds
	// stay baked.
	wantLifted(t, "MATCH p = (a {x: 1})-[:R]->{1,3}(b) RETURN p", "i1")
}

func TestLiftsBoolAndFloatInlineProps(t *testing.T) {
	wantLifted(t, "MATCH (a {active: true, score: 1.5}) RETURN a", "btrue", "f1.5")
}

// A minus over a numeric literal folds at parse time, so a negative inline
// prop, rel prop, and WHERE bound (int or float) lift exactly like their
// positive twins. Before the fold these parsed as Unary{Neg,Lit} and the
// literal-matching paths declined every one, lifting nothing (rcp b6a17c8).
func TestLiftsNegativeConstants(t *testing.T) {
	wantLifted(t,
		"MATCH (a:Acct {balance: -50})-[r {delta: -2}]->(b) "+
			"WHERE a.score < -5 AND b.lat = -2.35 RETURN a",
		"i-50", "i-2", "i-5", "f-2.35")
}

// The fold must not newly lift a constant deliberately baked into a
// projection/CASE (those shape the plan). A negative CASE threshold and its
// negative results stay put -- the expression walker skips literals.
func TestNegativeCaseConstantsStayBaked(t *testing.T) {
	wantLifted(t, "MATCH (a) RETURN CASE WHEN a.x < -5 THEN -1 ELSE -2 END AS c")
}

func TestLiftsExistsInProjectionPosition(t *testing.T) {
	// An EXISTS in a projection lifts through the expression walker (not
	// the WHERE walker).
	wantLifted(t, "MATCH (a) RETURN EXISTS { MATCH (a)-[:R]->(b {t: 'q'}) } AS e", "sq")
}

func TestProcCallArgsStayBaked(t *testing.T) {
	wantLifted(t, "CALL wcc('KNOWS') YIELD node, component RETURN node")
}

func TestBakedExpressionShapesRecurseWithoutLifting(t *testing.T) {
	// Prop-vs-prop comparisons, CASE, list predicates, index/slice,
	// property-of-expression, explicit named params, and FOR lists all
	// recurse but lift nothing.
	for _, q := range []string{
		"MATCH (a) WHERE a.x < a.y RETURN a",
		"MATCH (a) RETURN CASE a.x WHEN 1 THEN 'a' ELSE 'b' END AS c",
		"MATCH (a) RETURN CASE WHEN a.x > 1 THEN 'hi' ELSE 'lo' END AS c ORDER BY a.k",
		"MATCH (a) RETURN any(x IN [1, 2] WHERE x > 0) AS p",
		"MATCH (a) RETURN a.list[0] AS h",
		"MATCH (a) RETURN a.list[1..2] AS sl",
		"MATCH (a) RETURN coalesce(a.list).name AS h",
		"MATCH (a) RETURN a.name IN ['p', 'q'] AS m",
		"MATCH (a) RETURN toString(a.name) AS u, a.x IS NULL AS n",
		"MATCH (a) RETURN {k: 1} AS ml, -a.x AS neg",
		"MATCH (a) WHERE NOT a.flag RETURN a",
		"MATCH (a) WHERE a.id = $pid RETURN a",
		"FOR x IN [1, 2, 3] RETURN x",
	} {
		wantLifted(t, q)
	}
}

func TestTemplatesUnifyAcrossConstants(t *testing.T) {
	q1 := parse(t, "MATCH (p:Person {id: 669})-[:KNOWS]->(f) WHERE f.age > 30 RETURN f")
	q2 := parse(t, "MATCH (p:Person {id: 648})-[:KNOWS]->(f) WHERE f.age > 40 RETURN f")
	v1 := AutoParameterize(q1)
	v2 := AutoParameterize(q2)
	if !reflect.DeepEqual(q1, q2) {
		t.Fatal("templates must be identical after lifting")
	}
	if got := tags(v1); !reflect.DeepEqual(got, []string{"i669", "i30"}) {
		t.Fatalf("q1 values = %v", got)
	}
	if got := tags(v2); !reflect.DeepEqual(got, []string{"i648", "i40"}) {
		t.Fatalf("q2 values = %v", got)
	}
	// Lifting is idempotent: a second pass finds only params.
	if again := AutoParameterize(q1); len(again) != 0 {
		t.Fatalf("second pass lifted %v", tags(again))
	}
}

// Engine-only expression nodes (no GQL surface yet) still lift their
// nested subpattern constants: exercised by constructing the AST directly.
func TestEngineOnlyNodesLiftNestedSubpatterns(t *testing.T) {
	subq := func(tag string) *ast.Exists {
		return &ast.Exists{Pattern: &ast.Pattern{
			Start: ast.NodePat{Var: "b", Props: []ast.PropEntry{{Key: "t", Val: ast.StrLit(tag)}}},
		}}
	}
	q := &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{&ast.Match{Patterns: []ast.Pattern{{Start: ast.NodePat{Var: "a"}}}}},
		Ret: ast.Projection{Items: []ast.ReturnItem{
			{Expr: &ast.PatternComp{
				Pattern: &ast.Pattern{Start: ast.NodePat{Var: "c", Props: []ast.PropEntry{{Key: "x", Val: ast.IntLit(9)}}}},
				Where:   &ast.Binary{Op: ast.OpGt, LHS: &ast.Prop{Var: "c", Key: "n"}, RHS: &ast.Lit{Value: ast.IntLit(7)}},
				Proj:    &ast.Prop{Var: "c", Key: "name"},
			}, Alias: "xs"},
			{Expr: &ast.MapProj{Var: "a", Entries: []ast.MapProjEntry{
				{Kind: ast.MapProjField, Key: "e", Expr: subq("w")},
			}}, Alias: "mp"},
			{Expr: &ast.Reduce{Acc: "t", Init: &ast.Lit{Value: ast.IntLit(0)}, Var: "z",
				List: &ast.ListExpr{Elems: []ast.Expr{&ast.Lit{Value: ast.IntLit(1)}}},
				Body: subq("r")}, Alias: "rd"},
			{Expr: &ast.ListComp{Var: "y", List: &ast.Prop{Var: "a", Key: "xs"},
				Filter: subq("f"), Map: &ast.Var{Name: "y"}}, Alias: "lc"},
		}},
	}}}
	got := tags(AutoParameterize(q))
	want := []string{"i9", "i7", "sw", "sr", "sf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lifted %v, want %v", got, want)
	}
}
