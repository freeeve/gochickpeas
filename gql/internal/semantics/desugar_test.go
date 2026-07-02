// Desugar goldens: non-literal inline pattern properties lower to WHERE
// equality conjuncts (synthesizing variables for anonymous elements), the
// pass reaches subquery patterns, and it is idempotent.
package semantics

import (
	"errors"
	"reflect"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
)

func parse(t *testing.T, src string) *ast.Query {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return q
}

func desugared(t *testing.T, src string) *ast.Query {
	t.Helper()
	q := parse(t, src)
	if err := Desugar(q); err != nil {
		t.Fatalf("desugar %q: %v", src, err)
	}
	return q
}

// firstMatch returns the first Match clause of the query's first part.
func firstMatch(t *testing.T, q *ast.Query) *ast.Match {
	t.Helper()
	for _, c := range q.Parts[0].Clauses {
		if m, ok := c.(*ast.Match); ok {
			return m
		}
	}
	t.Fatal("no Match clause")
	return nil
}

// eqShape asserts e is Prop{v,key} = rhs and returns rhs.
func eqShape(t *testing.T, e ast.Expr, v, key string) ast.Expr {
	t.Helper()
	b, ok := e.(*ast.Binary)
	if !ok || b.Op != ast.OpEq {
		t.Fatalf("want equality conjunct, got %#v", e)
	}
	p, ok := b.LHS.(*ast.Prop)
	if !ok || p.Var != v || p.Key != key {
		t.Fatalf("want %s.%s on the left, got %#v", v, key, b.LHS)
	}
	return b.RHS
}

func TestLowersNodeInlineExprToWhereEquality(t *testing.T) {
	q := desugared(t, "MATCH (a:Person) MATCH (t:Tag {name: a.name}) RETURN t")
	var tags *ast.Match
	for _, c := range q.Parts[0].Clauses {
		if m, ok := c.(*ast.Match); ok {
			tags = m
		}
	}
	if len(tags.Patterns[0].Start.PropExprs) != 0 {
		t.Fatal("PropExprs must drain")
	}
	rhs := eqShape(t, tags.Where, "t", "name")
	if p, ok := rhs.(*ast.Prop); !ok || p.Var != "a" || p.Key != "name" {
		t.Fatalf("rhs = %#v", rhs)
	}
	// Literal inline props stay on the fast path.
	q = desugared(t, "MATCH (t:Tag {name: 'Hot'}) RETURN t")
	m := firstMatch(t, q)
	if m.Where != nil || len(m.Patterns[0].Start.Props) != 1 {
		t.Fatal("literal inline props stay put")
	}
}

func TestSynthesizesVarsForAnonymousElementsInOrder(t *testing.T) {
	q := desugared(t, "MATCH (a)-[:R]->({x: a.y})-[:S]->({z: a.w}) RETURN a")
	m := firstMatch(t, q)
	h := m.Patterns[0].Hops
	if h[0].Node.Var != "__ip0" || h[1].Node.Var != "__ip1" {
		t.Fatalf("synthetic vars = %q, %q", h[0].Node.Var, h[1].Node.Var)
	}
	// Both conjuncts landed, left-assoc AND, in pattern order.
	and, ok := m.Where.(*ast.Binary)
	if !ok || and.Op != ast.OpAnd {
		t.Fatalf("where = %#v", m.Where)
	}
	eqShape(t, and.LHS, "__ip0", "x")
	eqShape(t, and.RHS, "__ip1", "z")
}

func TestMergesIntoExistingWhereKeepingItFirst(t *testing.T) {
	q := desugared(t, "MATCH (a {k: a.z}) WHERE a.q > 1 RETURN a")
	m := firstMatch(t, q)
	and, ok := m.Where.(*ast.Binary)
	if !ok || and.Op != ast.OpAnd {
		t.Fatalf("where = %#v", m.Where)
	}
	if b, ok := and.LHS.(*ast.Binary); !ok || b.Op != ast.OpGt {
		t.Fatal("existing WHERE stays as the left conjunct")
	}
	eqShape(t, and.RHS, "a", "k")
}

func TestLowersRelInlineExprAndRejectsVarLength(t *testing.T) {
	q := desugared(t, "MATCH (a)-[r:R {since: a.y}]->(b) RETURN r")
	m := firstMatch(t, q)
	if len(m.Patterns[0].Hops[0].Rel.PropExprs) != 0 {
		t.Fatal("rel PropExprs must drain")
	}
	eqShape(t, m.Where, "r", "since")

	bad := parse(t, "MATCH (a)-[{w: a.y}]->{1,3}(b) RETURN a")
	err := Desugar(bad)
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindPlan {
		t.Fatalf("want a KindPlan error, got %v", err)
	}
}

func TestLowersInsideExistsSubquery(t *testing.T) {
	q := desugared(t, "MATCH (a) WHERE EXISTS { MATCH (a)-[:R]->(b {t: a.name}) } RETURN a")
	m := firstMatch(t, q)
	ex, ok := m.Where.(*ast.Exists)
	if !ok {
		t.Fatalf("where = %#v", m.Where)
	}
	if len(ex.Pattern.Hops[0].Node.PropExprs) != 0 {
		t.Fatal("subpattern PropExprs must drain")
	}
	eqShape(t, ex.Where, "b", "t")
}

func TestReachesProjectionUnwindAndCallSubquery(t *testing.T) {
	q := desugared(t,
		"MATCH (x) CALL { MATCH (m {v: x.name}) RETURN m } RETURN COUNT { MATCH (x)-[:R]->(c {q: x.z}) } AS n")
	var sub *ast.CallSubquery
	for _, c := range q.Parts[0].Clauses {
		if s, ok := c.(*ast.CallSubquery); ok {
			sub = s
		}
	}
	inner := firstMatch(t, &sub.Query)
	eqShape(t, inner.Where, "m", "v")
	cnt, ok := q.Parts[0].Ret.Items[0].Expr.(*ast.CountSub)
	if !ok {
		t.Fatalf("ret item = %#v", q.Parts[0].Ret.Items[0].Expr)
	}
	eqShape(t, cnt.Where, "c", "q")
}

func TestDesugarIsIdempotent(t *testing.T) {
	for _, src := range []string{
		"MATCH (a)-[:R]->({x: a.y}) RETURN a",
		"MATCH (a {k: a.z}) WHERE a.q > 1 AND EXISTS { MATCH (a)-[:R]->(b {t: a.name}) } RETURN a",
		"MATCH (t:Tag {name: 'Hot'}) FILTER t.n > 3 RETURN t ORDER BY t.n LIMIT 5",
		"FOR x IN [1, 2] RETURN CASE WHEN x > 1 THEN 'hi' ELSE 'lo' END AS c",
	} {
		q := desugared(t, src)
		again := desugared(t, src)
		if err := Desugar(again); err != nil {
			t.Fatalf("second desugar of %q: %v", src, err)
		}
		if !reflect.DeepEqual(q, again) {
			t.Fatalf("desugar not idempotent for %q", src)
		}
	}
}

func TestDesugarReachesRemainingClauseArms(t *testing.T) {
	// A projection boundary (LET), a FOR list, ORDER BY, and a path-search
	// clause all reach nested subquery patterns.
	q := desugared(t,
		"MATCH (a) LET e = EXISTS { MATCH (a)-[:R]->(b {t: a.name}) } "+
			"FOR x IN [1] RETURN e ORDER BY EXISTS { MATCH (a)-[:S]->(c {q: a.z}) }")
	var with *ast.With
	for _, c := range q.Parts[0].Clauses {
		if w, ok := c.(*ast.With); ok && len(w.Proj.Items) > 0 {
			with = w
		}
	}
	ex := with.Proj.Items[0].Expr.(*ast.Exists)
	eqShape(t, ex.Where, "b", "t")
	ord := q.Parts[0].Ret.OrderBy[0].Expr.(*ast.Exists)
	eqShape(t, ord.Where, "c", "q")

	sp := desugared(t, "MATCH p = ANY SHORTEST (a {k: a.z})-[:R]->+(b) RETURN p")
	for _, c := range sp.Parts[0].Clauses {
		if s, ok := c.(*ast.ShortestPath); ok {
			eqShape(t, s.Where, "a", "k")
			return
		}
	}
	t.Fatal("no ShortestPath clause")
}
