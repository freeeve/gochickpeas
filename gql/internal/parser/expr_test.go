// Expression-level parser tests: Pratt precedence, literals, postfix
// operators, subquery expressions, and the excluded-surface rejections.
package parser

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// whereOf parses src and returns the first MATCH's WHERE.
func whereOf(t *testing.T, src string) ast.Expr {
	t.Helper()
	_, where := oneMatch(t, mustParse(t, src))
	if where == nil {
		t.Fatalf("%s: no WHERE", src)
	}
	return where
}

// retExpr parses src and returns the first RETURN item.
func retExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	return mustParse(t, src).Parts[0].Ret.Items[0].Expr
}

func TestPrecedenceAndOrComparison(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE a.x > 1 AND a.y < 2 OR a.z = 3 RETURN a")
	or, ok := where.(*ast.Binary)
	if !ok || or.Op != ast.OpOr {
		t.Fatalf("root = %#v", where)
	}
	if l, ok := or.LHS.(*ast.Binary); !ok || l.Op != ast.OpAnd {
		t.Fatalf("lhs = %#v", or.LHS)
	}
	if r, ok := or.RHS.(*ast.Binary); !ok || r.Op != ast.OpEq {
		t.Fatalf("rhs = %#v", or.RHS)
	}
}

func TestParensOverridePrecedence(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE a.x AND (a.b OR a.c) RETURN a")
	and, ok := where.(*ast.Binary)
	if !ok || and.Op != ast.OpAnd {
		t.Fatalf("root = %#v", where)
	}
	if r, ok := and.RHS.(*ast.Binary); !ok || r.Op != ast.OpOr {
		t.Fatalf("rhs = %#v", and.RHS)
	}
}

func TestNotBindsLooserThanComparison(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE NOT a.x = 1 RETURN a")
	not, ok := where.(*ast.Unary)
	if !ok || not.Op != ast.Not {
		t.Fatalf("root = %#v", where)
	}
	if inner, ok := not.Expr.(*ast.Binary); !ok || inner.Op != ast.OpEq {
		t.Fatalf("operand = %#v", not.Expr)
	}
	// ...but tighter than AND.
	where2 := whereOf(t, "MATCH (a) WHERE NOT a.x AND a.y RETURN a")
	and := where2.(*ast.Binary)
	if and.Op != ast.OpAnd {
		t.Fatalf("root = %#v", where2)
	}
	if _, ok := and.LHS.(*ast.Unary); !ok {
		t.Fatalf("lhs = %#v", and.LHS)
	}
}

func TestUnaryMinusAndArithmetic(t *testing.T) {
	e := retExpr(t, "RETURN -1 * 2 + 3 AS x")
	// (((-1) * 2) + 3): unary minus binds tighter than *, * tighter than +.
	add := e.(*ast.Binary)
	if add.Op != ast.OpAdd {
		t.Fatalf("root = %#v", e)
	}
	mul := add.LHS.(*ast.Binary)
	if mul.Op != ast.OpMul {
		t.Fatalf("lhs = %#v", add.LHS)
	}
	// A minus over a numeric literal folds into the literal at parse time so
	// the constant-matching paths (prop seek, autoparam) see it (rcp b6a17c8).
	if lit, ok := mul.LHS.(*ast.Lit); !ok || lit.Value != ast.IntLit(-1) {
		t.Fatalf("mul lhs = %#v, want folded Lit(-1)", mul.LHS)
	}
	// A minus over a non-literal operand stays a runtime Unary, and still
	// binds tighter than * -- so precedence is observable there.
	e2 := retExpr(t, "RETURN -a.x * 2 AS y")
	mul2 := e2.(*ast.Binary)
	if mul2.Op != ast.OpMul {
		t.Fatalf("root2 = %#v", e2)
	}
	if neg, ok := mul2.LHS.(*ast.Unary); !ok || neg.Op != ast.Neg {
		t.Fatalf("mul2 lhs = %#v, want Unary Neg", mul2.LHS)
	}
}

func TestLiteralsAndDistinct(t *testing.T) {
	q := mustParse(t,
		"MATCH (a) WHERE a.ok = true AND a.bad = false AND a.gone = null AND a.r = 1.5 "+
			"RETURN DISTINCT a.name")
	if !q.Parts[0].Ret.Distinct {
		t.Fatal("distinct")
	}
	var lits []ast.Literal
	_, where := oneMatch(t, q)
	ast.Walk(where, func(e ast.Expr) bool {
		if l, ok := e.(*ast.Lit); ok {
			lits = append(lits, l.Value)
		}
		return true
	})
	want := map[ast.Literal]bool{
		ast.BoolLit(true): false, ast.BoolLit(false): false,
		ast.NullLit(): false, ast.FloatLit(1.5): false,
	}
	for _, l := range lits {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, seen := range want {
		if !seen {
			t.Fatalf("literal %+v not found in %v", l, lits)
		}
	}
}

func TestIdentWithKeywordPrefixIsNotAKeyword(t *testing.T) {
	e := retExpr(t, "MATCH (a) RETURN a.order_total")
	if p, ok := e.(*ast.Prop); !ok || p.Var != "a" || p.Key != "order_total" {
		t.Fatalf("expr = %#v", e)
	}
}

func TestInListAndCountDistinct(t *testing.T) {
	q := mustParse(t,
		"MATCH (p:Person) WHERE p.country IN ['US', 'UK'] RETURN count(DISTINCT p.city) AS cities")
	_, where := oneMatch(t, q)
	in, ok := where.(*ast.In)
	if !ok {
		t.Fatalf("where = %#v", where)
	}
	if p, ok := in.Expr.(*ast.Prop); !ok || p.Key != "country" {
		t.Fatalf("in expr = %#v", in.Expr)
	}
	list, ok := in.List.(*ast.ListExpr)
	if !ok || len(list.Elems) != 2 {
		t.Fatalf("in list = %#v", in.List)
	}
	f := q.Parts[0].Ret.Items[0].Expr.(*ast.Func)
	if f.Name != "count" || !f.Distinct || f.Star || len(f.Args) != 1 {
		t.Fatalf("func = %+v", f)
	}
}

func TestStringPredicates(t *testing.T) {
	for _, tc := range []struct {
		src string
		op  ast.BinOp
	}{
		{"MATCH (a) WHERE a.name STARTS WITH 'Al' RETURN a", ast.OpStartsWith},
		{"MATCH (a) WHERE a.name ENDS WITH 'ce' RETURN a", ast.OpEndsWith},
		{"MATCH (a) WHERE a.name CONTAINS 'lic' RETURN a", ast.OpContains},
	} {
		where := whereOf(t, tc.src)
		if b, ok := where.(*ast.Binary); !ok || b.Op != tc.op {
			t.Fatalf("%s: %#v", tc.src, where)
		}
	}
	// starts/ends/contains are not reserved: usable as identifiers.
	e := retExpr(t, "MATCH (a) RETURN a.starts")
	if p, ok := e.(*ast.Prop); !ok || p.Key != "starts" {
		t.Fatalf("expr = %#v", e)
	}
}

func TestIsNullPostfix(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE a.x IS NULL RETURN a")
	if n, ok := where.(*ast.IsNull); !ok || n.Negated {
		t.Fatalf("where = %#v", where)
	}
	where2 := whereOf(t, "MATCH (a) WHERE a.x IS NOT NULL RETURN a")
	if n, ok := where2.(*ast.IsNull); !ok || !n.Negated {
		t.Fatalf("where = %#v", where2)
	}
}

func TestIndexSliceAndPropOf(t *testing.T) {
	e := retExpr(t, "MATCH (a) RETURN a.xs[1] AS x")
	idx, ok := e.(*ast.Index)
	if !ok {
		t.Fatalf("expr = %#v", e)
	}
	if _, ok := idx.Base.(*ast.Prop); !ok {
		t.Fatalf("base = %#v", idx.Base)
	}
	for _, src := range []string{
		"MATCH (a) RETURN a.xs[1..2] AS x",
		"MATCH (a) RETURN a.xs[..2] AS x",
		"MATCH (a) RETURN a.xs[1..] AS x",
		"MATCH (a) RETURN a.xs[..] AS x",
	} {
		if _, ok := retExpr(t, src).(*ast.Slice); !ok {
			t.Fatalf("%s: not a slice", src)
		}
	}
	// Property access on a non-variable base is PropOf.
	e2 := retExpr(t, "MATCH (a) RETURN rels(a)[0].ts AS t")
	po, ok := e2.(*ast.PropOf)
	if !ok || po.Key != "ts" {
		t.Fatalf("expr = %#v", e2)
	}
}

func TestLabelPredicateExpression(t *testing.T) {
	where := whereOf(t, "MATCH (m) WHERE m:Comment RETURN m")
	hl, ok := where.(*ast.HasLabelExpr)
	if !ok || hl.Var != "m" || hl.Expr.Name != "Comment" {
		t.Fatalf("where = %#v", where)
	}
	mustErr(t, "MATCH (m) WHERE 1:Comment RETURN m", "must apply to a variable")
}

func TestCaseExpressions(t *testing.T) {
	e := retExpr(t, "MATCH (p:Person) RETURN CASE WHEN p.age < 30 THEN 'young' ELSE 'old' END AS bucket")
	c, ok := e.(*ast.Case)
	if !ok || c.Operand != nil || len(c.Whens) != 1 || c.Else == nil {
		t.Fatalf("searched = %#v", e)
	}
	e2 := retExpr(t, "MATCH (p) RETURN CASE p.x WHEN 1 THEN 'a' WHEN 2 THEN 'b' END AS r")
	c2, ok := e2.(*ast.Case)
	if !ok || c2.Operand == nil || len(c2.Whens) != 2 || c2.Else != nil {
		t.Fatalf("simple = %#v", e2)
	}
}

func TestExistsAndCountSubqueries(t *testing.T) {
	where := whereOf(t, "MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(f) } RETURN p.name")
	ex, ok := where.(*ast.Exists)
	if !ok || ex.Pattern.Start.Var != "p" || len(ex.Pattern.Hops) != 1 || ex.Where != nil {
		t.Fatalf("exists = %#v", where)
	}
	where2 := whereOf(t,
		"MATCH (p:Person) WHERE NOT EXISTS { MATCH (p)-[:WORKS_AT]->(c) WHERE c.name = 'Acme' } RETURN p.name")
	not, ok := where2.(*ast.Unary)
	if !ok || not.Op != ast.Not {
		t.Fatalf("not exists = %#v", where2)
	}
	if inner, ok := not.Expr.(*ast.Exists); !ok || inner.Where == nil {
		t.Fatalf("inner = %#v", not.Expr)
	}
	e := retExpr(t, "MATCH (p) RETURN COUNT { MATCH (p)-[:KNOWS]->(f) } AS friends")
	cs, ok := e.(*ast.CountSub)
	if !ok || len(cs.Pattern.Hops) != 1 {
		t.Fatalf("count sub = %#v", e)
	}
	// count(...) stays the aggregate.
	e2 := retExpr(t, "MATCH (p) RETURN count(p) AS n")
	if _, ok := e2.(*ast.Func); !ok {
		t.Fatalf("count fn = %#v", e2)
	}
}

func TestListPredicates(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE all(x IN a.xs WHERE x > 0) RETURN a")
	lp, ok := where.(*ast.ListPred)
	if !ok || lp.Quant != ast.QuantAll || lp.Var != "x" {
		t.Fatalf("all = %#v", where)
	}
	where2 := whereOf(t, "MATCH (a) WHERE single(y IN a.ys WHERE y = 1) RETURN a")
	if lp2, ok := where2.(*ast.ListPred); !ok || lp2.Quant != ast.QuantSingle {
		t.Fatalf("single = %#v", where2)
	}
	// A quantifier name without the `var IN` head is a plain function.
	e := retExpr(t, "MATCH (a) RETURN any(a.xs) AS x")
	if _, ok := e.(*ast.Func); !ok {
		t.Fatalf("any() = %#v", e)
	}
}

func TestMapLiterals(t *testing.T) {
	e := retExpr(t, "RETURN {name: 'Alice', age: 30} AS m")
	m, ok := e.(*ast.MapLit)
	if !ok || len(m.Fields) != 2 || m.Fields[0].Key != "name" || m.Fields[1].Key != "age" {
		t.Fatalf("map = %#v", e)
	}
	e2 := retExpr(t, "RETURN {} AS m")
	if m2, ok := e2.(*ast.MapLit); !ok || len(m2.Fields) != 0 {
		t.Fatalf("empty map = %#v", e2)
	}
	e3 := retExpr(t, "MATCH (p) RETURN {x: p.age} AS b")
	if _, ok := e3.(*ast.MapLit); !ok {
		t.Fatalf("computed map = %#v", e3)
	}
}

func TestParamsParse(t *testing.T) {
	where := whereOf(t, "MATCH (a) WHERE a.id = $id RETURN a")
	bin := where.(*ast.Binary)
	l, ok := bin.RHS.(*ast.Lit)
	if !ok || l.Value != ast.NamedParamLit("id") {
		t.Fatalf("param = %#v", bin.RHS)
	}
	// $end is fine -- param names skip the reserved-word check.
	where2 := whereOf(t, "MATCH (a) WHERE a.x = $end RETURN a")
	if l2 := where2.(*ast.Binary).RHS.(*ast.Lit); l2.Value != ast.NamedParamLit("end") {
		t.Fatalf("param = %#v", where2)
	}
}

func TestStringsNoEscapeProcessing(t *testing.T) {
	e := retExpr(t, `RETURN 'a\nb' AS s`)
	l := e.(*ast.Lit)
	if l.Value != ast.StrLit(`a\nb`) {
		t.Fatalf("string kept raw: %+v", l.Value)
	}
	e2 := retExpr(t, `RETURN "double 'quoted'" AS s`)
	if e2.(*ast.Lit).Value != ast.StrLit("double 'quoted'") {
		t.Fatalf("double-quoted = %+v", e2)
	}
	mustErr(t, "RETURN 'unterminated", "unterminated string")
}

func TestExcludedSurfaceRejections(t *testing.T) {
	mustErr(t, "RETURN reduce(total = 0, x IN [1] | total + x) AS s", "reduce")
	mustErr(t, "MATCH (p) RETURN p{.name} AS m", "map projections")
	mustErr(t, "MATCH (p) RETURN p{.*} AS m", "map projections")
}

// TestListComprehension covers the [x IN xs [WHERE p] [| m]] extension:
// a leading `ident IN` after '[' opens a comprehension (the Rust
// grammar's ordered choice); all three optional-part combinations parse.
func TestListComprehension(t *testing.T) {
	e := retExpr(t, "MATCH (a) RETURN [x IN a.xs WHERE x > 1 | x * 2] AS ys")
	lc, ok := e.(*ast.ListComp)
	if !ok {
		t.Fatalf("expected ListComp, got %#v", e)
	}
	if lc.Var != "x" || lc.Filter == nil || lc.Map == nil {
		t.Fatalf("comprehension parts: %+v", lc)
	}
	e2 := retExpr(t, "MATCH (a) RETURN [x IN a.xs WHERE x > 1] AS ys")
	if lc2 := e2.(*ast.ListComp); lc2.Filter == nil || lc2.Map != nil {
		t.Fatalf("filter-only comprehension: %+v", lc2)
	}
	e3 := retExpr(t, "MATCH (a) RETURN [r IN rels(a) | r.ts] AS ts")
	if lc3 := e3.(*ast.ListComp); lc3.Filter != nil || lc3.Map == nil {
		t.Fatalf("map-only comprehension: %+v", lc3)
	}
	// A parenthesized membership test stays a list literal element.
	e4 := retExpr(t, "MATCH (a) RETURN [(1 IN a.xs), 2] AS ys")
	if _, isList := e4.(*ast.ListExpr); !isList {
		t.Fatalf("expected list literal, got %#v", e4)
	}
}

func TestReservedWordsRejectedAsIdents(t *testing.T) {
	mustErr(t, "MATCH (match) RETURN 1", "reserved word")
	mustErr(t, "MATCH (a) RETURN a AS order", "reserved word")
	mustErr(t, "MATCH (a)-[filter:R]->(b) RETURN a", "reserved word")
}
