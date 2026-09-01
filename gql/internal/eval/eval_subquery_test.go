package eval

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
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

// TestSubqueryRelVarWhere pins the walk's rel-variable binding: a WHERE
// (or comprehension projection) reading a rel var bound by the subquery
// pattern must see the traversed relationship, not null. Before the
// relSlots fix every such read evaluated null and silently rejected the
// row -- an EXISTS with both endpoints bound and a rel-prop filter
// returned false on a matching edge.
func TestSubqueryRelVarWhere(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	alice, _ := b.AddNode("Person")
	bob, _ := b.AddNode("Person")
	carol, _ := b.AddNode("Person")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	r1, err := b.AddRel(alice, bob, "KNOWS")
	must(err)
	must(b.SetRelPropAt(r1, "w", int64(5)))
	r2, err := b.AddRel(alice, bob, "KNOWS")
	must(err)
	must(b.SetRelPropAt(r2, "w", int64(1)))
	r3, err := b.AddRel(bob, carol, "KNOWS")
	must(err)
	must(b.SetRelPropAt(r3, "w", int64(1)))
	ctx := &Ctx{G: graph.New(b.Finalize())}
	row := []value.Value{value.Node(0), value.Node(1)}
	slots := map[string]int{"a": 0, "b": 1}

	// One endpoint bound.
	oneBound := exprOf(t, "EXISTS { MATCH (a)-[k:KNOWS]->(x) WHERE k.w > 3 }")
	if got, _ := Eval(ctx, oneBound, row, slots).AsBool(); !got {
		t.Fatal("alice has a w=5 edge; rel-var WHERE read null")
	}
	bobRow := []value.Value{value.Node(1), value.Node(2)}
	if got, _ := Eval(ctx, oneBound, bobRow, slots).AsBool(); got {
		t.Fatal("bob's only edge is w=1; filter must reject")
	}
	// Both endpoints bound (the rel-enumeration path replaces the
	// count-only fast path).
	bothBound := exprOf(t, "EXISTS { MATCH (a)-[k:KNOWS]->(b) WHERE k.w > 3 }")
	if got, _ := Eval(ctx, bothBound, row, slots).AsBool(); !got {
		t.Fatal("both-bound EXISTS with rel-prop filter missed the w=5 edge")
	}
	// Undirected spelling.
	undir := exprOf(t, "EXISTS { MATCH (a)-[k:KNOWS]-(b) WHERE k.w > 3 }")
	if got, _ := Eval(ctx, undir, row, slots).AsBool(); !got {
		t.Fatal("undirected both-bound EXISTS with rel-prop filter missed")
	}
	// COUNT multiplicity: both parallel edges pass w > 0, one passes w > 3.
	cntAll := exprOf(t, "COUNT { MATCH (a)-[k:KNOWS]->(b) WHERE k.w > 0 }")
	if got, _ := Eval(ctx, cntAll, row, slots).AsInt(); got != 2 {
		t.Fatalf("parallel-edge count = %d, want 2", got)
	}
	cntBig := exprOf(t, "COUNT { MATCH (a)-[k:KNOWS]->(b) WHERE k.w > 3 }")
	if got, _ := Eval(ctx, cntBig, row, slots).AsInt(); got != 1 {
		t.Fatalf("filtered count = %d, want 1", got)
	}
	// Pattern comprehension projecting the rel property.
	pc := &ast.PatternComp{
		Pattern: &ast.Pattern{
			Start: ast.NodePat{Var: "a"},
			Hops: []ast.PatternHop{{
				Rel:  ast.RelPat{Var: "k", Dir: ast.DirOut, Types: []string{"KNOWS"}},
				Node: ast.NodePat{Var: "x"},
			}},
		},
		Proj: &ast.Prop{Var: "k", Key: "w"},
	}
	got := Eval(ctx, pc, row, slots)
	lst, ok := got.AsList()
	if !ok || len(lst) != 2 {
		t.Fatalf("comprehension = %v, want two weights", got)
	}
	sum := int64(0)
	for _, v := range lst {
		n, _ := v.AsInt()
		sum += n
	}
	if sum != 6 {
		t.Fatalf("projected weights sum = %d, want 6 (5+1)", sum)
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
