// Direct branch coverage for the compile package's helpers that the
// end-to-end gql tests reach only partially: the const-expression walker
// (list-scope bound variables included), the memo key packer, the
// compiled property read's non-entity branches, the keyed prop fallback
// decoder, the slot walker over every cnode, and the IN hoists' three
// membership representations.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

func coverGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	a, _ := b.AddNode("N")
	c, _ := b.AddNode("N")
	_ = b.SetProp(a, "i", int64(7))
	_ = b.SetProp(a, "f", 2.5)
	_ = b.SetProp(a, "b", true)
	_ = b.SetProp(a, "s", "hello")
	if _, err := b.AddRel(a, c, "R"); err != nil {
		t.Fatal(err)
	}
	return b.Finalize()
}

func lit(i int64) ast.Expr { return &ast.Lit{Value: ast.IntLit(i)} }

// TestConstExprForms walks every constExpr branch, including the
// list-scope forms whose iteration variables bind through boundWith.
func TestConstExprForms(t *testing.T) {
	cases := []struct {
		name string
		e    ast.Expr
		want bool
	}{
		{"lit", lit(1), true},
		{"free var", &ast.Var{Name: "x"}, false},
		{"unary", &ast.Unary{Op: ast.Not, Expr: lit(1)}, true},
		{"binary", &ast.Binary{Op: ast.OpAdd, LHS: lit(1), RHS: lit(2)}, true},
		{"list", &ast.ListExpr{Elems: []ast.Expr{lit(1), &ast.Var{Name: "x"}}}, false},
		{"maplit", &ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: lit(1)}}}, true},
		{"in", &ast.In{Expr: lit(1), List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}}}, true},
		{"isnull", &ast.IsNull{Expr: lit(1)}, true},
		{"index", &ast.Index{Base: &ast.ListExpr{Elems: []ast.Expr{lit(1)}}, Idx: lit(0)}, true},
		{"slice open", &ast.Slice{Base: &ast.ListExpr{}}, true},
		{"slice bounds", &ast.Slice{Base: &ast.ListExpr{}, From: lit(0), To: lit(1)}, true},
		{"propof", &ast.PropOf{Base: &ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: lit(1)}}}, Key: "k"}, true},
		{"case", &ast.Case{Operand: lit(1), Whens: []ast.CaseWhen{{Cond: lit(1), Result: lit(2)}}, Else: lit(3)}, true},
		{"func pure", &ast.Func{Name: "abs", Args: []ast.Expr{lit(-1)}}, true},
		{"func unknown", &ast.Func{Name: "nope", Args: []ast.Expr{lit(1)}}, false},
		{"func distinct", &ast.Func{Name: "abs", Distinct: true, Args: []ast.Expr{lit(1)}}, false},
		{"listpred bound", &ast.ListPred{Quant: ast.QuantAll, Var: "y",
			List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}},
			Pred: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "y"}, RHS: lit(0)}}, true},
		{"listpred free", &ast.ListPred{Quant: ast.QuantAll, Var: "y",
			List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}},
			Pred: &ast.Var{Name: "z"}}, false},
		{"reduce bound", &ast.Reduce{Acc: "s", Init: lit(0), Var: "y",
			List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}},
			Body: &ast.Binary{Op: ast.OpAdd, LHS: &ast.Var{Name: "s"}, RHS: &ast.Var{Name: "y"}}}, true},
		{"listcomp bound", &ast.ListComp{Var: "y",
			List:   &ast.ListExpr{Elems: []ast.Expr{lit(1)}},
			Filter: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "y"}, RHS: lit(0)},
			Map:    &ast.Var{Name: "y"}}, true},
		{"pattern form", &ast.Exists{Pattern: &ast.Pattern{}}, false},
	}
	for _, tc := range cases {
		if got := constExpr(tc.e, nil); got != tc.want {
			t.Errorf("%s: constExpr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPackNodeKey covers all packer arms: one node, two nodes, a
// non-node slot, and an unpackable arity.
func TestPackNodeKey(t *testing.T) {
	row := []value.Value{value.Node(3), value.Node(9), value.Str("x")}
	if k, ok := packNodeKey([]int{0}, row); !ok || k != 3 {
		t.Fatalf("one node: %d %v", k, ok)
	}
	if k, ok := packNodeKey([]int{0, 1}, row); !ok || k != 3<<32|9 {
		t.Fatalf("two nodes: %d %v", k, ok)
	}
	if _, ok := packNodeKey([]int{2}, row); ok {
		t.Fatal("non-node packed")
	}
	if _, ok := packNodeKey([]int{0, 2}, row); ok {
		t.Fatal("mixed pair packed")
	}
	if _, ok := packNodeKey([]int{0, 1, 0}, row); ok {
		t.Fatal("triple packed")
	}
}

// TestCevalPropBranches drives the compiled property read's non-entity
// arms: map fields, temporal components over epoch ints and temporal
// values, and the null fallbacks.
func TestCevalPropBranches(t *testing.T) {
	g := coverGraph(t)
	p := &cProp{slot: 0, reader: newPropReader(g, "k")}
	mapRow := []value.Value{value.Map([]value.MapEntry{{Key: "k", Val: value.Int(5)}})}
	if v := cevalProp(p, mapRow); func() int64 { i, _ := v.AsInt(); return i }() != 5 {
		t.Fatalf("map field: %v", v)
	}
	missRow := []value.Value{value.Map([]value.MapEntry{{Key: "other", Val: value.Int(5)}})}
	if v := cevalProp(p, missRow); !v.IsNull() {
		t.Fatalf("missing map field: %v", v)
	}
	year := &cProp{slot: 0, reader: newPropReader(g, "year")}
	epoch := []value.Value{value.Int(1268638245000)} // 2010-03-15
	if v := cevalProp(year, epoch); func() int64 { i, _ := v.AsInt(); return i }() != 2010 {
		t.Fatalf("int year: %v", v)
	}
	temporal := []value.Value{value.Temporal(1268638245000, value.DateTime)}
	if v := cevalProp(year, temporal); func() int64 { i, _ := v.AsInt(); return i }() != 2010 {
		t.Fatalf("temporal year: %v", v)
	}
	if v := cevalProp(year, []value.Value{value.Bool(true)}); !v.IsNull() {
		t.Fatalf("bool base: %v", v)
	}
	if v := cevalProp(&cProp{slot: 5, reader: newPropReader(g, "k")}, mapRow); !v.IsNull() {
		t.Fatalf("wide slot: %v", v)
	}
	if v := cevalProp(&cProp{slot: -1, reader: newPropReader(g, "k")}, mapRow); !v.IsNull() {
		t.Fatalf("negative slot: %v", v)
	}
}

// TestPropValueKinds decodes every keyed-fallback property kind.
func TestPropValueKinds(t *testing.T) {
	g := coverGraph(t)
	for key, want := range map[string]value.Value{
		"i": value.Int(7), "f": value.Float(2.5), "b": value.Bool(true), "s": value.Str("hello"),
	} {
		got := propValue(g.Prop(0, key))
		if o, ok := value.Compare(got, want); !ok || o != 0 {
			t.Errorf("prop %q = %v, want %v", key, got, want)
		}
	}
	if v := propValue(g.Prop(0, "absent")); !v.IsNull() {
		t.Errorf("absent prop: %v", v)
	}
}

// TestHoistMembershipForms compiles IN over batch-constant lists of each
// membership representation (node set, hashed scalars, linear fallback
// with an unhashable element) plus the carried form, and evaluates each
// through ceval for result parity with the plain IN.
func TestHoistMembershipForms(t *testing.T) {
	g := coverGraph(t)
	ctx := &eval.Ctx{}
	slots := map[string]int{"x": 0, "c": 1}
	isConst := func(s int) bool { return s == 1 }
	sample := []value.Value{value.Null(), value.List([]value.Value{value.Int(1), value.Int(2)})}

	build := func(list ast.Expr) *Compiled {
		return New(ctx, &ast.In{Expr: &ast.Var{Name: "x"}, List: list}, slots, g)
	}
	probe := func(c *Compiled, row []value.Value) value.Value {
		return c.Eval(ctx, row, slots)
	}

	// Batch-constant carried slot -> cInConst with a hashed-scalar set.
	hoisted := HoistConstIn(ctx, build(&ast.Var{Name: "c"}), isConst, sample, slots)
	if v := probe(hoisted, []value.Value{value.Int(2), sample[1]}); !v.IsTruthy() {
		t.Fatalf("hashed membership hit: %v", v)
	}
	if v := probe(hoisted, []value.Value{value.Int(9), sample[1]}); v.IsTruthy() {
		t.Fatalf("hashed membership miss: %v", v)
	}

	// Node list -> the entity bitmap representation.
	nodes := value.List([]value.Value{value.Node(0), value.Node(1)})
	sampleN := []value.Value{value.Null(), nodes}
	hoistedN := HoistConstIn(ctx, build(&ast.Var{Name: "c"}), isConst, sampleN, slots)
	if v := probe(hoistedN, []value.Value{value.Node(1), nodes}); !v.IsTruthy() {
		t.Fatalf("node membership hit: %v", v)
	}

	// A null element forces the linear representation and null-on-miss.
	withNull := value.List([]value.Value{value.Int(1), value.Null()})
	sampleL := []value.Value{value.Null(), withNull}
	hoistedL := HoistConstIn(ctx, build(&ast.Var{Name: "c"}), isConst, sampleL, slots)
	if v := probe(hoistedL, []value.Value{value.Int(9), withNull}); !v.IsNull() {
		t.Fatalf("null-list miss should be null: %v", v)
	}

	// Carried (loop-invariant, not batch-constant) -> cInCarried.
	carried := HoistCarriedIn(build(&ast.Var{Name: "c"}), func(s int) bool { return s == 1 })
	if v := probe(carried, []value.Value{value.Int(1), sample[1]}); !v.IsTruthy() {
		t.Fatalf("carried membership hit: %v", v)
	}

	// Slots over a tree containing the fused comparison node.
	fused := New(ctx, &ast.Binary{Op: ast.OpLt,
		LHS: &ast.Prop{Var: "x", Key: "i"}, RHS: lit(10)}, slots, g)
	refs, hasSlow, hasWalk := Slots(fused)
	if hasSlow || hasWalk || len(refs) != 1 || refs[0] != 0 {
		t.Fatalf("fused slots = %v, hasSlow %v hasWalk %v", refs, hasSlow, hasWalk)
	}
}

// TestCorrelatedSlots covers the outer-reads walker's arms, including
// the nested-subquery and pattern-comprehension bailouts.
func TestCorrelatedSlots(t *testing.T) {
	outer := map[string]int{"a": 0, "b": 1}
	pat := &ast.Pattern{
		Start: ast.NodePat{Var: "a"},
		Hops:  []ast.PatternHop{{Rel: ast.RelPat{Types: []string{"R"}}, Node: ast.NodePat{Var: "m"}}},
	}
	where := &ast.Binary{Op: ast.OpEq,
		LHS: &ast.Prop{Var: "m", Key: "k"}, RHS: &ast.Var{Name: "b"}}
	ms, ok := correlatedSlots(pat, where, outer)
	if !ok || len(ms) != 2 {
		t.Fatalf("correlated = %v ok=%v, want both outer slots", ms, ok)
	}
	nested := &ast.Exists{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "z"}}}
	if _, ok := correlatedSlots(pat, nested, outer); ok {
		t.Fatal("nested subquery should disable the memo")
	}

	// Every remaining collector arm, driven as WHERE conjuncts.
	wide := &ast.Binary{Op: ast.OpAnd,
		LHS: &ast.Unary{Op: ast.Not, Expr: &ast.IsNull{Expr: &ast.Var{Name: "b"}}},
		RHS: &ast.Binary{Op: ast.OpAnd,
			LHS: &ast.In{Expr: &ast.Var{Name: "b"},
				List: &ast.ListExpr{Elems: []ast.Expr{lit(1), &ast.Var{Name: "a"}}}},
			RHS: &ast.Binary{Op: ast.OpAnd,
				LHS: &ast.ListPred{Quant: ast.QuantAny, Var: "y",
					List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}}, Pred: &ast.Var{Name: "b"}},
				RHS: &ast.Binary{Op: ast.OpAnd,
					LHS: &ast.Reduce{Acc: "s", Init: lit(0), Var: "y",
						List: &ast.ListExpr{Elems: []ast.Expr{lit(1)}}, Body: &ast.Var{Name: "b"}},
					RHS: &ast.ListComp{Var: "y",
						List:   &ast.ListExpr{Elems: []ast.Expr{lit(1)}},
						Filter: &ast.Var{Name: "b"}, Map: &ast.Var{Name: "y"}},
				},
			},
		},
	}
	if _, ok := correlatedSlots(pat, wide, outer); !ok {
		t.Fatal("wide conjunct should stay memoizable")
	}
	cost := &ast.Cost{From: "a", To: "b"}
	if _, ok := correlatedSlots(pat, cost, outer); !ok {
		t.Fatal("cost form should collect both endpoints")
	}
	pcomp := &ast.PatternComp{Pattern: &ast.Pattern{Start: ast.NodePat{Var: "z"}}, Proj: &ast.Var{Name: "z"}}
	if _, ok := correlatedSlots(pat, pcomp, outer); ok {
		t.Fatal("pattern comprehension should disable the memo")
	}
}

// TestSlotWalkerAndHoistRecursion drives slotsOf, hoistConst, and
// hoistCarried through every structural cnode arm at once: a case
// expression wrapping negation, null tests, lists, and IN forms, hoisted
// under both the batch-constant and carried rules.
func TestSlotWalkerAndHoistRecursion(t *testing.T) {
	g := coverGraph(t)
	ctx := &eval.Ctx{}
	slots := map[string]int{"x": 0, "c": 1}
	e := &ast.Case{
		Whens: []ast.CaseWhen{{
			Cond: &ast.Unary{Op: ast.Not, Expr: &ast.IsNull{Expr: &ast.Unary{Op: ast.Neg, Expr: &ast.Var{Name: "x"}}}},
			Result: &ast.In{Expr: &ast.Var{Name: "x"},
				List: &ast.ListExpr{Elems: []ast.Expr{&ast.Var{Name: "c"}, lit(2)}}},
		}},
		Else: &ast.ListExpr{Elems: []ast.Expr{&ast.Var{Name: "x"}}},
	}
	c := New(ctx, e, slots, g)
	refs, hasSlow, _ := Slots(c)
	if hasSlow || len(refs) == 0 {
		t.Fatalf("slots = %v hasSlow=%v", refs, hasSlow)
	}
	sample := []value.Value{value.Int(2), value.List([]value.Value{value.Int(2)})}
	hoisted := HoistConstIn(ctx, c, func(s int) bool { return s == 1 }, sample, slots)
	row := []value.Value{value.Int(2), sample[1]}
	if v := hoisted.Eval(ctx, row, slots); !v.IsTruthy() {
		t.Fatalf("hoisted case = %v", v)
	}
	carried := HoistCarriedIn(c, func(s int) bool { return s == 1 })
	if v := carried.Eval(ctx, row, slots); !v.IsTruthy() {
		t.Fatalf("carried case = %v", v)
	}
	// Same value, second epoch: the carried rebuild short-circuit.
	if v := carried.Eval(ctx, row, slots); !v.IsTruthy() {
		t.Fatalf("carried case epoch 2 = %v", v)
	}
}
