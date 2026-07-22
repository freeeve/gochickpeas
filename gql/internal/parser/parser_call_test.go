// Parser tests for the CALL forms: the procedure call (CALL ... YIELD ->
// CallProc) and the correlated/uncorrelated call subquery (CALL (imports)
// { ... } -> CallSubquery). Split from parser_test.go, which keeps the
// pattern- and clause-level statement tests.
package parser

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func TestCallProcYield(t *testing.T) {
	q := mustParse(t, "CALL wcc('replyOf') YIELD node, component AS c RETURN c")
	call, ok := q.Parts[0].Clauses[0].(*ast.CallProc)
	lit0, ok0 := (ast.Expr)(nil), false
	if ok && len(call.Args) == 1 {
		var l *ast.Lit
		if l, ok0 = call.Args[0].(*ast.Lit); ok0 {
			lit0 = l
			ok0 = l.Value == ast.StrLit("replyOf")
		}
	}
	if !ok || call.Proc != "wcc" || !ok0 {
		t.Fatalf("call = %#v lit = %#v", q.Parts[0].Clauses[0], lit0)
	}
	if len(call.Yields) != 2 || call.Yields[0] != (ast.YieldItem{Field: "node"}) ||
		call.Yields[1] != (ast.YieldItem{Field: "component", Alias: "c"}) {
		t.Fatalf("yields = %+v", call.Yields)
	}
	// Dotted names; a negative numeric arg folds to a constant literal at
	// parse time (rcp b6a17c8), the same shape a positive arg already takes.
	q2 := mustParse(t, "CALL geo.withinRadius('Place', 48.8, -2.35, 5.0) YIELD node RETURN node")
	call2 := q2.Parts[0].Clauses[0].(*ast.CallProc)
	lit2, okLit := call2.Args[2].(*ast.Lit)
	if call2.Proc != "geo.withinRadius" || !okLit || lit2.Value != ast.FloatLit(-2.35) {
		t.Fatalf("call2 = %+v", call2)
	}
}

func TestCallSubquery(t *testing.T) {
	// Correlated: the scope clause names the imports and a synthesized
	// importing With leads the body.
	q := mustParse(t,
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS friends } "+
			"RETURN p.name AS name, friends")
	cs, ok := q.Parts[0].Clauses[1].(*ast.CallSubquery)
	if !ok || len(cs.Imports) != 1 || cs.Imports[0] != "p" {
		t.Fatalf("subquery = %#v", q.Parts[0].Clauses[1])
	}
	if len(cs.Query.Parts) != 1 || len(cs.Query.Union) != 0 {
		t.Fatalf("body = %+v", cs.Query)
	}
	w, ok := cs.Query.Parts[0].Clauses[0].(*ast.With)
	if !ok || len(w.Proj.Items) != 1 {
		t.Fatalf("import with = %#v", cs.Query.Parts[0].Clauses[0])
	}
	if v, ok := w.Proj.Items[0].Expr.(*ast.Var); !ok || v.Name != "p" {
		t.Fatalf("import var = %#v", w.Proj.Items[0].Expr)
	}
	// Uncorrelated: no scope clause, no imports.
	q2 := mustParse(t, "CALL { MATCH (c:Company) RETURN count(c) AS c } RETURN c")
	cs2 := q2.Parts[0].Clauses[0].(*ast.CallSubquery)
	if len(cs2.Imports) != 0 {
		t.Fatalf("imports = %v", cs2.Imports)
	}
	// A UNION body imports into every branch.
	q3 := mustParse(t,
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN f AS n "+
			"UNION MATCH (p)-[:WORKS_AT]->(c) RETURN c AS n } RETURN n")
	cs3 := q3.Parts[0].Clauses[1].(*ast.CallSubquery)
	if len(cs3.Query.Parts) != 2 || cs3.Query.Union[0] != ast.UnionDistinct {
		t.Fatalf("union body = %+v", cs3.Query)
	}
	for i := range cs3.Query.Parts {
		if _, ok := cs3.Query.Parts[i].Clauses[0].(*ast.With); !ok {
			t.Fatalf("branch %d missing import with", i)
		}
	}
	// The procedure form still parses as CallProc.
	q4 := mustParse(t, "CALL wcc('KNOWS') YIELD node, component RETURN node")
	if _, ok := q4.Parts[0].Clauses[0].(*ast.CallProc); !ok {
		t.Fatal("procedure form")
	}
}
