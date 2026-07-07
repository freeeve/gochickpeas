// Ported from the Rust engine's eval_functions.rs at the expression level:
// each case parses `RETURN <expr> AS x` (or builds engine-only AST nodes
// directly), evaluates through Eval, and asserts the value -- pinning
// behavior ahead of the executor milestone.
package eval

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/value"
)

// exprOf parses `RETURN <src> AS x` and returns the projected expression.
func exprOf(t *testing.T, src string) ast.Expr {
	t.Helper()
	q, err := parser.Parse("RETURN " + src + " AS x")
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return q.Parts[0].Ret.Items[0].Expr
}

// testGraph is Alice(0)-KNOWS->Bob(1) with name/age/score/active props.
func testGraph(t *testing.T) *Ctx {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	alice, _ := b.AddNode("Person")
	bob, _ := b.AddNode("Person")
	_ = b.SetProp(alice, "name", "Alice")
	_ = b.SetProp(alice, "age", int64(30))
	_ = b.SetProp(alice, "score", 2.5)
	_ = b.SetProp(bob, "name", "Bob")
	_ = b.SetProp(bob, "age", int64(40))
	if _, err := b.AddRel(alice, bob, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	return &Ctx{G: graph.New(b.Finalize())}
}

// ev evaluates src with no row bindings.
func ev(t *testing.T, ctx *Ctx, src string) value.Value {
	t.Helper()
	return Eval(ctx, exprOf(t, src), nil, nil)
}

func wantInt(t *testing.T, ctx *Ctx, src string, want int64) {
	t.Helper()
	if got, ok := ev(t, ctx, src).AsInt(); !ok || got != want {
		t.Fatalf("%s = (%d, %v), want %d", src, got, ok, want)
	}
}

func wantFloat(t *testing.T, ctx *Ctx, src string, want float64) {
	t.Helper()
	v := ev(t, ctx, src)
	if got, ok := v.AsFloat(); !ok || v.Kind() != value.KindFloat || got != want {
		t.Fatalf("%s = %v, want Float %v", src, v, want)
	}
}

func wantStr(t *testing.T, ctx *Ctx, src, want string) {
	t.Helper()
	if got, ok := ev(t, ctx, src).AsStr(); !ok || got != want {
		t.Fatalf("%s = (%q, %v), want %q", src, got, ok, want)
	}
}

func wantBool(t *testing.T, ctx *Ctx, src string, want bool) {
	t.Helper()
	if got, ok := ev(t, ctx, src).AsBool(); !ok || got != want {
		t.Fatalf("%s = (%v, %v), want %v", src, got, ok, want)
	}
}

func wantNull(t *testing.T, ctx *Ctx, src string) {
	t.Helper()
	if v := ev(t, ctx, src); !v.IsNull() {
		t.Fatalf("%s = %v, want Null", src, v)
	}
}

func wantInts(t *testing.T, ctx *Ctx, src string, want ...int64) {
	t.Helper()
	xs, ok := ev(t, ctx, src).AsList()
	if !ok || len(xs) != len(want) {
		t.Fatalf("%s: got %v (ok=%v), want %v", src, xs, ok, want)
	}
	for i, x := range xs {
		if got, _ := x.AsInt(); got != want[i] {
			t.Fatalf("%s[%d] = %d, want %d", src, i, got, want[i])
		}
	}
}

func TestMathFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "abs(-3)", 3)
	wantFloat(t, g, "abs(-2.5)", 2.5)
	wantFloat(t, g, "ceil(2.1)", 3.0)
	wantFloat(t, g, "floor(2.9)", 2.0)
	wantFloat(t, g, "round(2.5)", 3.0)
	wantFloat(t, g, "sqrt(9.0)", 3.0)
	wantInt(t, g, "sign(-5)", -1)
	wantInt(t, g, "sign(0)", 0)
	wantInt(t, g, "sign(7)", 1)
	wantNull(t, g, "sqrt('nope')")
}

func TestStringFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "size('hello')", 5)
	wantStr(t, g, "left('hello', 3)", "hel")
	wantStr(t, g, "right('hello', 2)", "lo")
	wantStr(t, g, "substring('hello', 1, 3)", "ell")
	wantStr(t, g, "substring('hello', 2)", "llo")
	wantNull(t, g, "substring('hello', 1, -1)")
	wantStr(t, g, "'foo' + 'bar'", "foobar")
	wantBool(t, g, "'hello' STARTS WITH 'he'", true)
	wantBool(t, g, "'hello' ENDS WITH 'lo'", true)
	wantBool(t, g, "'hello' CONTAINS 'ell'", true)
	wantNull(t, g, "'hello' CONTAINS 3")
	wantStr(t, g, "lower('FootBALL')", "football")
	wantStr(t, g, "upper('FootBALL')", "FOOTBALL")
	wantStr(t, g, "toLower('FootBALL')", "football")
	wantStr(t, g, "toUpper('FootBALL')", "FOOTBALL")
	wantBool(t, g, "lower('The Football Cup') CONTAINS 'football'", true)
	wantNull(t, g, "lower(3)")
	wantNull(t, g, "upper(null)")
}

func TestConversionFunctions(t *testing.T) {
	g := testGraph(t)
	wantStr(t, g, "toString(42)", "42")
	wantStr(t, g, "toString(1.0)", "1.0")
	wantStr(t, g, "toString(true)", "true")
	wantStr(t, g, "toString(2.5)", "2.5")
	wantInt(t, g, "toInteger('3.9')", 3)
	wantInt(t, g, "toInteger(3.9)", 3)
	wantFloat(t, g, "toFloat('2.5')", 2.5)
	wantBool(t, g, "toBoolean('TRUE')", true)
	wantBool(t, g, "toBoolean('false')", false)
	wantNull(t, g, "toBoolean('nope')")
	wantNull(t, g, "toInteger('abc')")
}

func TestDurationArithmetic(t *testing.T) {
	g := testGraph(t)
	// Int +/- Duration reads the Int as epoch millis and shifts it,
	// staying an Int (BI Q17's creationDate + duration(...)).
	wantInt(t, g, "1000 + duration({seconds: 4})", 5000)
	wantInt(t, g, "duration({seconds: 4}) + 1000", 5000)
	wantInt(t, g, "10000 - duration({seconds: 4})", 6000)
	wantBool(t, g, "5000 > 1000 + duration({seconds: 1})", true)
	// Temporal - Temporal -> exact millisecond duration.
	wantBool(t, g, "zoned_datetime('2011-07-20') - zoned_datetime('2011-07-19') = duration({hours: 24})", true)
	// Duration +/- Duration, componentwise.
	wantBool(t, g, "duration({hours: 1}) + duration({minutes: 30}) = duration({minutes: 90})", true)
	wantBool(t, g, "duration({hours: 2}) - duration({minutes: 30}) = duration({minutes: 90})", true)
	// Duration scaling by an integral factor (commutative *, exact /).
	wantBool(t, g, "duration({hours: 2}) * 3 = duration({hours: 6})", true)
	wantBool(t, g, "3 * duration({hours: 2}) = duration({hours: 6})", true)
	wantBool(t, g, "duration({hours: 4}) / 2 = duration({hours: 2})", true)
	wantNull(t, g, "duration({hours: 4}) / 0")
	wantNull(t, g, "duration({hours: 4}) * 1.5")
}

func TestListConcat(t *testing.T) {
	g := testGraph(t)
	// list + list chains, list + element appends, element + list prepends.
	wantInts(t, g, "[1, 2] + [3]", 1, 2, 3)
	wantInts(t, g, "[1, 2] + 3", 1, 2, 3)
	wantInts(t, g, "1 + [2, 3]", 1, 2, 3)
	wantInts(t, g, "[] + [1] + []", 1)
	wantInt(t, g, "size([1] + [2, 3])", 3)
	// A null operand stays null; numeric + is untouched.
	wantNull(t, g, "[1] + null")
	wantNull(t, g, "null + [1]")
	wantInt(t, g, "1 + 2", 3)
	wantStr(t, g, "'a' + 'b'", "ab")
}

func TestListAndRangeFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "size([1, 2, 3])", 3)
	wantInts(t, g, "range(1, 5)", 1, 2, 3, 4, 5)
	wantInts(t, g, "range(0, 10, 2)", 0, 2, 4, 6, 8, 10)
	wantInts(t, g, "range(5, 1, -2)", 5, 3, 1)
	wantNull(t, g, "range(1, 5, 0)")
	wantInt(t, g, "[10, 20, 30][1]", 20)
	wantInt(t, g, "[10, 20, 30][-1]", 30)
	wantNull(t, g, "[10, 20][5]")
	wantInts(t, g, "[1, 2, 3, 4][1..3]", 2, 3)
	wantInts(t, g, "[1, 2, 3, 4][..2]", 1, 2)
	wantInts(t, g, "[1, 2, 3, 4][-2..]", 3, 4)
}

func TestMembershipAndNullPredicates(t *testing.T) {
	g := testGraph(t)
	wantBool(t, g, "2 IN [1, 2, 3]", true)
	wantBool(t, g, "5 IN [1, 2, 3]", false)
	wantBool(t, g, "null IS NULL", true)
	wantBool(t, g, "1 IS NULL", false)
	wantBool(t, g, "1 IS NOT NULL", true)
	wantNull(t, g, "null IN [1, 2]")
}

func TestIntegerOverflowYieldsNullNotPanic(t *testing.T) {
	g := testGraph(t)
	wantNull(t, g, "-9223372036854775808 / -1")
	wantNull(t, g, "abs(-9223372036854775808)")
	wantNull(t, g, "9223372036854775807 + 1")
	wantNull(t, g, "9223372036854775807 * 2")
	wantNull(t, g, "1 / 0")
	wantNull(t, g, "1.0 / 0.0")
	wantInt(t, g, "2 + 3", 5)
	wantInt(t, g, "6 / 2", 3)
	wantInt(t, g, "7 / 2", 3)
	wantFloat(t, g, "7.0 / 2", 3.5)
}

func TestThreeValuedLogicAndOrNot(t *testing.T) {
	g := testGraph(t)
	wantNull(t, g, "true AND null")
	wantNull(t, g, "null AND true")
	wantBool(t, g, "false AND null", false)
	wantBool(t, g, "null AND false", false)
	wantBool(t, g, "true AND true", true)
	wantBool(t, g, "null OR true", true)
	wantBool(t, g, "true OR null", true)
	wantNull(t, g, "null OR false")
	wantNull(t, g, "null OR null")
	wantBool(t, g, "false OR false", false)
	// NOT over a null-yielding AND must stay null.
	wantNull(t, g, "NOT (true AND null)")
	wantNull(t, g, "NOT null")
}

func TestThreeValuedInAndQuantifiers(t *testing.T) {
	g := testGraph(t)
	wantNull(t, g, "3 IN [1, null]")
	wantBool(t, g, "3 IN [3, null]", true)
	wantBool(t, g, "3 IN [1, 2]", false)
	wantNull(t, g, "all(y IN [1, null] WHERE y > 0)")
	wantBool(t, g, "all(y IN [1, null] WHERE y > 5)", false)
	wantBool(t, g, "any(y IN [1, null] WHERE y > 0)", true)
	wantNull(t, g, "any(y IN [1, null] WHERE y > 5)")
	wantNull(t, g, "none(y IN [1, null] WHERE y > 5)")
}

func TestCaseAndCoalesce(t *testing.T) {
	g := testGraph(t)
	wantStr(t, g, "CASE WHEN 1 > 2 THEN 'a' ELSE 'b' END", "b")
	wantStr(t, g, "CASE 2 WHEN 1 THEN 'x' WHEN 2 THEN 'y' ELSE 'z' END", "y")
	wantNull(t, g, "CASE WHEN false THEN 'a' END")
	wantInt(t, g, "coalesce(null, null, 7)", 7)
	wantStr(t, g, "coalesce(null, 'a')", "a")
	wantNull(t, g, "coalesce(null, null)")
}

func TestListPredicates(t *testing.T) {
	g := testGraph(t)
	wantBool(t, g, "all(y IN [1, 2, 3] WHERE y > 0)", true)
	wantBool(t, g, "all(y IN [1, 2, 3] WHERE y > 1)", false)
	wantBool(t, g, "any(y IN [1, 2] WHERE y > 1)", true)
	wantBool(t, g, "none(y IN [1, 2] WHERE y > 5)", true)
	wantBool(t, g, "single(y IN [1, 2, 3] WHERE y = 2)", true)
	wantBool(t, g, "single(y IN [2, 2] WHERE y = 2)", false)
	// Empty-list folds: all/none true, any/single false.
	wantBool(t, g, "all(y IN [] WHERE y > 0)", true)
	wantBool(t, g, "none(y IN [] WHERE y > 0)", true)
	wantBool(t, g, "any(y IN [] WHERE y > 0)", false)
	wantBool(t, g, "single(y IN [] WHERE y > 0)", false)
	wantNull(t, g, "all(y IN null WHERE y > 0)")
}

// TestReduceAndListComp exercises the engine-only nodes (no GQL surface)
// via directly-built AST.
func TestReduceAndListComp(t *testing.T) {
	g := testGraph(t)
	list := &ast.ListExpr{Elems: []ast.Expr{
		&ast.Lit{Value: ast.IntLit(1)}, &ast.Lit{Value: ast.IntLit(2)},
		&ast.Lit{Value: ast.IntLit(3)}, &ast.Lit{Value: ast.IntLit(4)},
	}}
	red := &ast.Reduce{
		Acc: "s", Init: &ast.Lit{Value: ast.IntLit(0)}, Var: "y", List: list,
		Body: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "s"}, RHS: &ast.Var{Name: "y"}},
	}
	if got, _ := Eval(g, red, nil, nil).AsInt(); got != 10 {
		t.Fatalf("reduce = %d, want 10", got)
	}
	comp := &ast.ListComp{
		Var: "y", List: list,
		Filter: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "y"}, RHS: &ast.Lit{Value: ast.IntLit(2)}},
		Map:    &ast.Binary{Op: ast.OpMul, LHS: &ast.Var{Name: "y"}, RHS: &ast.Lit{Value: ast.IntLit(10)}},
	}
	xs, _ := Eval(g, comp, nil, nil).AsList()
	if len(xs) != 2 {
		t.Fatalf("list comp = %v", xs)
	}
	if a, _ := xs[0].AsInt(); a != 30 {
		t.Fatalf("comp[0] = %d", a)
	}
	if b, _ := xs[1].AsInt(); b != 40 {
		t.Fatalf("comp[1] = %d", b)
	}
	// Filter-only and map-only forms.
	fOnly := &ast.ListComp{Var: "y", List: list, Filter: comp.Filter}
	if xs, _ := Eval(g, fOnly, nil, nil).AsList(); len(xs) != 2 {
		t.Fatalf("filter-only comp = %v", xs)
	}
	wantNullComp := &ast.ListComp{Var: "y", List: &ast.Lit{Value: ast.NullLit()}}
	if !Eval(g, wantNullComp, nil, nil).IsNull() {
		t.Fatal("comp over null is null")
	}
}

func TestGraphResolvedFunctions(t *testing.T) {
	g := testGraph(t)
	// Bind p -> Alice, r -> the KNOWS rel position.
	var pos uint32
	for _, p := range g.G.Relationships(0, graph.Outgoing, []string{"KNOWS"}) {
		pos = p
	}
	row := []value.Value{value.Node(0), value.Rel(pos)}
	slots := map[string]int{"p": 0, "r": 1}
	evalIn := func(src string) value.Value { return Eval(g, exprOf(t, src), row, slots) }
	if got, _ := evalIn("id(p)").AsInt(); got != 0 {
		t.Fatalf("id(p) = %d", got)
	}
	if got, _ := evalIn("id(startNode(r))").AsInt(); got != 0 {
		t.Fatalf("id(startNode(r)) = %d", got)
	}
	if got, _ := evalIn("id(endNode(r))").AsInt(); got != 1 {
		t.Fatalf("id(endNode(r)) = %d", got)
	}
	if !evalIn("startNode(p)").IsNull() {
		t.Fatal("startNode of a non-rel is null")
	}
	// Property reads and unary over them.
	if got, _ := evalIn("p.age").AsInt(); got != 30 {
		t.Fatalf("p.age = %d", got)
	}
	if got, _ := evalIn("-p.age").AsInt(); got != -30 {
		t.Fatalf("-p.age = %d", got)
	}
	if got := evalIn("-p.score"); func() float64 { f, _ := got.AsFloat(); return f }() != -2.5 {
		t.Fatalf("-p.score = %v", got)
	}
	if got, _ := evalIn("p.name").AsStr(); got != "Alice" {
		t.Fatalf("p.name = %q", got)
	}
	if !evalIn("p.nope").IsNull() {
		t.Fatal("missing prop is null")
	}
	// Label predicate postfix.
	if got, _ := evalIn("p:Person").AsBool(); !got {
		t.Fatal("p:Person")
	}
	if !Eval(g, exprOf(t, "q:Person"), row, slots).IsNull() {
		t.Fatal("unbound var label predicate is null")
	}
	// Path functions over a constructed path value.
	path := value.Path([]uint32{0, 1}, []uint32{pos})
	prow := []value.Value{path}
	pslots := map[string]int{"pp": 0}
	if got, _ := Eval(g, exprOf(t, "length(pp)"), prow, pslots).AsInt(); got != 1 {
		t.Fatalf("length(path) = %d", got)
	}
	if got, _ := Eval(g, exprOf(t, "size(nodes(pp))"), prow, pslots).AsInt(); got != 2 {
		t.Fatalf("size(nodes(path)) = %d", got)
	}
	if got, _ := Eval(g, exprOf(t, "size(rels(pp))"), prow, pslots).AsInt(); got != 1 {
		t.Fatalf("size(rels(path)) = %d", got)
	}
}

func TestTemporalFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "date('2020-03-15')", 20200315)
	wantInt(t, g, "datetime('2020-03-15T10:30:00').year", 2020)
	wantInt(t, g, "datetime('2020-03-15T10:30:00').month", 3)
	wantInt(t, g, "datetime('2020-03-15T10:30:00').day", 15)
	wantInt(t, g, "datetime('2020-03-15T10:30:00').hour", 10)
	wantInt(t, g, "datetime({epochMillis: 1584267000000}).epochMillis", 1584267000000)
	wantNull(t, g, "date('nope')")
	// Component maps and temporal arithmetic.
	wantInt(t, g, "datetime({year: 2020, month: 3, day: 15}).day", 15)
	wantInt(t, g, "(datetime('2020-01-31') + duration({months: 1})).day", 29)
	wantInt(t, g, "(datetime('2020-03-15') - duration({days: 20})).month", 2)
	// Comparison coercion: temporal vs epoch-millis integer.
	wantBool(t, g, "datetime({epochMillis: 100}) < 200", true)
	// An i64 reads temporal components directly (the accessor is the type
	// signal).
	wantInt(t, g, "datetime('1970-01-02').epochMillis", 86400000)
}

func TestMapLiteralAndProjection(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "{a: 1, b: 2}.a", 1)
	wantNull(t, g, "{a: 1}.z")
	// Map projection (engine-only surface): p{.name, .age} + computed.
	proj := &ast.MapProj{Var: "p", Entries: []ast.MapProjEntry{
		{Kind: ast.MapProjProp, Key: "name"},
		{Kind: ast.MapProjField, Key: "twice", Expr: &ast.Binary{
			Op: ast.OpMul, LHS: &ast.Prop{Var: "p", Key: "age"}, RHS: &ast.Lit{Value: ast.IntLit(2)},
		}},
	}}
	row := []value.Value{value.Node(0)}
	slots := map[string]int{"p": 0}
	m, ok := Eval(g, proj, row, slots).AsMap()
	if !ok || len(m) != 2 {
		t.Fatalf("map proj = %v", m)
	}
	if s, _ := m[0].Val.AsStr(); m[0].Key != "name" || s != "Alice" {
		t.Fatalf("proj name = %v", m[0])
	}
	if i, _ := m[1].Val.AsInt(); m[1].Key != "twice" || i != 60 {
		t.Fatalf("proj twice = %v", m[1])
	}
	// .* expands every property in ascending key order.
	all := &ast.MapProj{Var: "p", Entries: []ast.MapProjEntry{{Kind: ast.MapProjAll}}}
	am, _ := Eval(g, all, row, slots).AsMap()
	if len(am) != 3 || am[0].Key != "age" || am[1].Key != "name" || am[2].Key != "score" {
		t.Fatalf(".* = %v", am)
	}
}

func TestExistsCountAndPatternComp(t *testing.T) {
	g := testGraph(t)
	row := []value.Value{value.Node(0)}
	slots := map[string]int{"a": 0}
	// EXISTS { MATCH (a)-[:KNOWS]->(b) } with a bound.
	ex := exprOf(t, "EXISTS { MATCH (a)-[:KNOWS]->(b) }")
	if got, _ := Eval(g, ex, row, slots).AsBool(); !got {
		t.Fatal("alice knows someone")
	}
	// Reversed anchoring: only the far end is bound.
	rev := exprOf(t, "EXISTS { MATCH (z)-[:KNOWS]->(a) }")
	bobRow := []value.Value{value.Node(1)}
	if got, _ := Eval(g, rev, bobRow, slots).AsBool(); !got {
		t.Fatal("someone knows bob (reversed anchor)")
	}
	if got, _ := Eval(g, rev, row, slots).AsBool(); got {
		t.Fatal("nobody knows alice")
	}
	// COUNT subquery + inner WHERE.
	cnt := exprOf(t, "COUNT { MATCH (a)-[:KNOWS]->(b) WHERE b.age > 35 }")
	if got, _ := Eval(g, cnt, row, slots).AsInt(); got != 1 {
		t.Fatalf("count = %d", got)
	}
	cnt0 := exprOf(t, "COUNT { MATCH (a)-[:KNOWS]->(b) WHERE b.age > 99 }")
	if got, _ := Eval(g, cnt0, row, slots).AsInt(); got != 0 {
		t.Fatalf("count = %d", got)
	}
	// Unanchored subquery scans by label.
	free := exprOf(t, "COUNT { MATCH (x:Person)-[:KNOWS]->(y) }")
	if got, _ := Eval(g, free, nil, nil).AsInt(); got != 1 {
		t.Fatalf("free count = %d", got)
	}
	// An outer variable bound to null (OPTIONAL MATCH miss) never matches.
	nullRow := []value.Value{value.Null()}
	if got, _ := Eval(g, ex, nullRow, slots).AsBool(); got {
		t.Fatal("null-bound anchor cannot match")
	}
	// Pattern comprehension (engine-only surface): [(a)-[:KNOWS]->(b) | b.name].
	pc := &ast.PatternComp{
		Pattern: &ast.Pattern{
			Start: ast.NodePat{Var: "a"},
			Hops: []ast.PatternHop{{
				Rel:  ast.RelPat{Dir: ast.DirOut, Types: []string{"KNOWS"}},
				Node: ast.NodePat{Var: "b"},
			}},
		},
		Proj: &ast.Prop{Var: "b", Key: "name"},
	}
	xs, _ := Eval(g, pc, row, slots).AsList()
	if len(xs) != 1 {
		t.Fatalf("pattern comp = %v", xs)
	}
	if s, _ := xs[0].AsStr(); s != "Bob" {
		t.Fatalf("pattern comp[0] = %q", s)
	}
}

func TestParamsResolveThroughCtx(t *testing.T) {
	g := testGraph(t)
	g.Params = []value.Value{value.Int(42)}
	g.Named = map[string]value.Value{"name": value.Str("Alice")}
	// Named params parse from the surface; auto slots are built directly.
	if got, _ := ev(t, g, "$name").AsStr(); got != "Alice" {
		t.Fatalf("$name = %q", got)
	}
	wantNull(t, g, "$missing")
	slot := &ast.Lit{Value: ast.ParamLit(0)}
	if got, _ := Eval(g, slot, nil, nil).AsInt(); got != 42 {
		t.Fatalf("slot 0 = %d", got)
	}
	if !Eval(g, &ast.Lit{Value: ast.ParamLit(9)}, nil, nil).IsNull() {
		t.Fatal("out-of-range slot is null")
	}
}

func TestOperatorsAndPrecedence(t *testing.T) {
	g := testGraph(t)
	wantBool(t, g, "NOT (1 > 2)", true)
	wantInt(t, g, "3 + 4 * 2", 11)
	wantInt(t, g, "(3 + 4) * 2", 14)
	wantFloat(t, g, "2.5 * 2", 5.0)
	wantBool(t, g, "1 < 2 AND 2 < 3", true)
	wantNull(t, g, "'a' < 1")
	// Unary minus of MinInt64 literal parses as neg(9223372036854775808)
	// which overflows i64 at parse or eval -- either way not a panic.
	if v := ev(t, g, "-(9223372036854775807)"); v.IsNull() {
		t.Fatal("negating MaxInt64 is fine")
	}
}

func TestToStringEdgeFormats(t *testing.T) {
	if s, _ := ApplyFunc(FuncToString, []value.Value{value.Float(math.Inf(1))}).AsStr(); s != "inf" {
		t.Fatalf("inf = %q", s)
	}
	if s, _ := ApplyFunc(FuncToString, []value.Value{value.Float(math.NaN())}).AsStr(); s != "NaN" {
		t.Fatalf("NaN = %q", s)
	}
	if s, _ := ApplyFunc(FuncToString, []value.Value{value.Float(-0.5)}).AsStr(); s != "-0.5" {
		t.Fatalf("-0.5 = %q", s)
	}
	if !ApplyFunc(FuncToString, []value.Value{value.Null()}).IsNull() {
		t.Fatal("toString(null) is null")
	}
}
