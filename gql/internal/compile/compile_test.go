// White-box tests for the compile lowering: constant folding, membership
// index selection and exact IN semantics, const/carried hoisting, epoch
// caching, subquery memo slots, and the Slots pushdown analysis.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/value"
)

// fixture is Alice(0)-KNOWS->Bob(1) with name/age props.
func fixture(t *testing.T) (*eval.Ctx, *chickpeas.Snapshot) {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	alice, _ := b.AddNode("Person")
	bob, _ := b.AddNode("Person")
	_ = b.SetProp(alice, "name", "Alice")
	_ = b.SetProp(alice, "age", int64(30))
	_ = b.SetProp(bob, "name", "Bob")
	_ = b.SetProp(bob, "age", int64(40))
	if _, err := b.AddRel(alice, bob, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize("name")
	return &eval.Ctx{G: graph.New(g)}, g
}

// exprOf parses `RETURN <src> AS x` and returns the projected expression.
func exprOf(t *testing.T, src string) ast.Expr {
	t.Helper()
	q, err := parser.Parse("RETURN " + src + " AS x")
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return q.Parts[0].Ret.Items[0].Expr
}

func TestConstantFunctionFolding(t *testing.T) {
	ctx, g := fixture(t)
	c := New(ctx, exprOf(t, "datetime('2020-03-15').year"), nil, g)
	// The datetime constructor folds; .year is a PropOf over it, which
	// stays slow -- so fold the constructor itself.
	inner := New(ctx, exprOf(t, "datetime('2020-03-15')"), nil, g)
	if _, ok := inner.c.(*cLit); !ok {
		t.Fatalf("constant datetime should fold to a literal, got %T", inner.c)
	}
	if v := c.Eval(ctx, nil, nil); !value.Equal(v, value.Int(2020)) {
		t.Fatalf("folded year = %v", v)
	}
	// Non-constant args stay a compiled function.
	nc := New(ctx, exprOf(t, "abs(x)"), map[string]int{"x": 0}, g)
	if _, ok := nc.c.(*cFunc); !ok {
		t.Fatalf("abs(x) should compile to a function node, got %T", nc.c)
	}
}

func TestMembershipRepresentations(t *testing.T) {
	nodes := buildMembership([]value.Value{value.Node(1), value.Node(3)})
	if nodes.kind != memNodes || !nodes.resultFor(value.Node(3)).IsTruthy() {
		t.Fatal("all-node list uses the bitmap")
	}
	if nodes.resultFor(value.Int(3)).IsTruthy() {
		t.Fatal("an int probe never hits a node bitmap")
	}
	scalars := buildMembership([]value.Value{value.Int(1), value.Str("a"), value.Float(2.5)})
	if scalars.kind != memHash {
		t.Fatal("hashable scalars use the hash set")
	}
	// Integral float probe hits the equal integer (Equal coercion).
	if !scalars.resultFor(value.Float(1.0)).IsTruthy() {
		t.Fatal("1.0 IN [1, ...] holds")
	}
	// A miss over a null-free list is a definite false.
	if v := scalars.resultFor(value.Int(9)); v.IsTruthy() || v.IsNull() {
		t.Fatal("miss over null-free list is false")
	}
	withNull := buildMembership([]value.Value{value.Int(1), value.Null()})
	if withNull.kind != memLinear || !withNull.hasNull {
		t.Fatal("a null element forces the linear fallback")
	}
	if !withNull.resultFor(value.Int(9)).IsNull() {
		t.Fatal("miss over a null-element list is null")
	}
	// Temporals coerce against ints under Equal -- they must scan linearly.
	temporal := buildMembership([]value.Value{value.Temporal(100, value.DateTime)})
	if temporal.kind != memLinear {
		t.Fatal("temporal elements force the linear fallback")
	}
	if !temporal.resultFor(value.Int(100)).IsTruthy() {
		t.Fatal("Int(100) IN [Temporal(100)] holds via Equal coercion")
	}
}

func TestHoistConstAndCarried(t *testing.T) {
	ctx, g := fixture(t)
	slots := map[string]int{"x": 0, "carried": 1}
	// A pure-literal list bakes to a constant membership index.
	c := New(ctx, exprOf(t, "x IN [1, 2, 3]"), slots, g)
	hc := HoistConstIn(ctx, c, func(int) bool { return false }, nil, slots)
	if _, ok := hc.c.(*cInConst); !ok {
		t.Fatalf("literal IN list should bake, got %T", hc.c)
	}
	row := []value.Value{value.Int(2), value.Null()}
	if !hc.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("2 IN [1,2,3]")
	}
	// A carried-slot list becomes the per-epoch cached form.
	c = New(ctx, exprOf(t, "x IN carried"), slots, g)
	cc := HoistCarriedIn(c, func(s int) bool { return s == 1 })
	carried, ok := cc.c.(*cInCarried)
	if !ok {
		t.Fatalf("carried IN list should cache per call, got %T", cc.c)
	}
	ctx.MatchEpoch = 7
	row = []value.Value{value.Int(2), value.List([]value.Value{value.Int(2), value.Int(9)})}
	if !cc.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("2 IN carried")
	}
	if carried.epoch != 7 || !carried.built {
		t.Fatal("cache built for the current epoch")
	}
	// Same epoch reuses the cache even if the slot changes (loop-invariant
	// within one call by contract); a new epoch rebuilds.
	row[1] = value.List([]value.Value{value.Int(5)})
	if !cc.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("same epoch reuses the built set")
	}
	ctx.MatchEpoch = 8
	if cc.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("new epoch rebuilds from the new list")
	}
	// A non-list carried value mirrors IN's null.
	ctx.MatchEpoch = 9
	row[1] = value.Int(3)
	if !cc.Eval(ctx, row, slots).IsNull() {
		t.Fatal("carried non-list is null")
	}
}

func TestSubqueryMemoAndSlots(t *testing.T) {
	ctx, g := fixture(t)
	slots := map[string]int{"a": 0}
	e := exprOf(t, "EXISTS { MATCH (a)-[:KNOWS]->(b) }")
	c := New(ctx, e, slots, g)
	sub, ok := c.c.(*cSubquery)
	if !ok {
		t.Fatalf("EXISTS compiles to a subquery node, got %T", c.c)
	}
	if !sub.hasMemo || len(sub.memoSlots) != 1 || sub.memoSlots[0] != 0 {
		t.Fatalf("memo slots = %v (hasMemo %v)", sub.memoSlots, sub.hasMemo)
	}
	row := []value.Value{value.Node(0)}
	if !c.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("alice knows bob")
	}
	// A node-valued correlated slot memoizes via the entity-id fast path
	// (memoI), leaving the byte-string memo empty.
	if sub.memoI.Len() != 1 || len(sub.memo) != 0 {
		t.Fatal("result memoized")
	}
	// Memoized result reused for the same correlated binding.
	if !c.Eval(ctx, row, slots).IsTruthy() || sub.memoI.Len() != 1 {
		t.Fatal("memo hit")
	}
	// Slots: the memoized subquery pushes down to its correlated slots --
	// and reports the walk, so placement can respect walk cost (task 115).
	refs, slow, walk := Slots(c)
	if slow || !walk || len(refs) != 1 || refs[0] != 0 {
		t.Fatalf("subquery pushdown refs = %v slow = %v walk = %v", refs, slow, walk)
	}
	// A function keeps last-level placement.
	refs, slow, walk = Slots(New(ctx, exprOf(t, "size(a)"), slots, g))
	if !slow {
		t.Fatalf("function is slow, refs = %v walk = %v", refs, walk)
	}
}

func TestPropReaderKindsAndAbsents(t *testing.T) {
	ctx, g := fixture(t)
	slots := map[string]int{"p": 0}
	c := New(ctx, exprOf(t, "p.name"), slots, g)
	if v := c.Eval(ctx, []value.Value{value.Node(0)}, slots); !value.Equal(v, value.Str("Alice")) {
		t.Fatalf("p.name = %v", v)
	}
	if v := c.Eval(ctx, []value.Value{value.Node(1)}, slots); !value.Equal(v, value.Str("Bob")) {
		t.Fatalf("bob name = %v", v)
	}
	c = New(ctx, exprOf(t, "p.nosuch"), slots, g)
	if !c.Eval(ctx, []value.Value{value.Node(0)}, slots).IsNull() {
		t.Fatal("missing key is null")
	}
	// A rel property read through the rel reader.
	var pos uint32
	for _, p := range ctx.G.Relationships(0, graph.Outgoing, []string{"KNOWS"}) {
		pos = p
	}
	c = New(ctx, exprOf(t, "r.nosuch"), map[string]int{"r": 0}, g)
	if !c.Eval(ctx, []value.Value{value.Rel(pos)}, map[string]int{"r": 0}).IsNull() {
		t.Fatal("missing rel prop is null")
	}
	// Map and temporal bases route like the interpreter.
	c = New(ctx, exprOf(t, "p.year"), slots, g)
	if v := c.Eval(ctx, []value.Value{value.Int(86400000)}, slots); !value.Equal(v, value.Int(1970)) {
		t.Fatalf("epoch-millis .year = %v", v)
	}
}
