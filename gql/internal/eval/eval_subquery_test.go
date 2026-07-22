package eval

import (
	"math"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Correlated-subquery, parameter, operator-precedence, and toString-edge
// evaluation tests. Split from eval_test.go (which keeps the shared
// Ctx/graph fixtures, want-helpers, and the scalar/logic/list tests).

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
