// Ported from the Rust cypher crate's value.rs tests: the semantics tables
// for Compare/Equal, OrderCmp totality, group keys, and the Kleene
// combinators must match the Rust engine exactly.
package value

import (
	"math"
	"sort"
	"testing"
)

func cmpEq(t *testing.T, a, b Value, want int) {
	t.Helper()
	got, ok := Compare(a, b)
	if !ok || got != want {
		t.Fatalf("Compare(%v, %v) = (%d, %v), want (%d, true)", a, b, got, ok, want)
	}
}

func cmpNone(t *testing.T, a, b Value) {
	t.Helper()
	if _, ok := Compare(a, b); ok {
		t.Fatalf("Compare(%v, %v) comparable, want incomparable", a, b)
	}
}

func TestCompareScalarsAndNumericCoercion(t *testing.T) {
	cmpEq(t, Int(1), Int(2), -1)
	cmpEq(t, Int(2), Float(2.0), 0)
	cmpEq(t, Float(3.5), Int(3), 1)
	cmpEq(t, Bool(false), Bool(true), -1)
	cmpEq(t, Str("a"), Str("b"), -1)
	cmpEq(t, Node(3), Node(3), 0)
	cmpEq(t, Rel(9), Rel(4), 1)
	cmpNone(t, Null(), Int(1))
	cmpNone(t, Int(1), Null())
	cmpNone(t, Str("x"), Int(1))
	cmpNone(t, Float(math.NaN()), Float(1.0))
}

func TestCompareListsElementwiseThenLength(t *testing.T) {
	a := List([]Value{Int(1), Int(2)})
	b := List([]Value{Int(1), Int(3)})
	cmpEq(t, a, b, -1)
	c := List([]Value{Int(1)})
	d := List([]Value{Int(1), Int(9)})
	cmpEq(t, c, d, -1)
	e := List([]Value{Int(1), Int(2)})
	if !Equal(a, e) {
		t.Fatal("equal lists must be Equal")
	}
}

func TestCompareMapsEqualButUnordered(t *testing.T) {
	m1 := Map([]MapEntry{{"a", Int(1)}, {"b", Int(2)}})
	m2 := Map([]MapEntry{{"b", Int(2)}, {"a", Int(1)}})
	cmpEq(t, m1, m2, 0)
	if !Equal(m1, m2) {
		t.Fatal("order-independent map equality")
	}
	m3 := Map([]MapEntry{{"a", Int(1)}, {"b", Int(9)}})
	cmpNone(t, m1, m3)
	m4 := Map([]MapEntry{{"a", Int(1)}})
	if Equal(m1, m4) {
		t.Fatal("different key sets are unequal")
	}
}

func TestCompareTemporalAndDuration(t *testing.T) {
	t1 := Temporal(100, Date)
	t2 := Temporal(200, DateTime)
	cmpEq(t, t1, t2, -1)
	cmpEq(t, t1, Int(100), 0)
	cmpEq(t, Int(50), t1, -1)
	d1 := Duration(1, 2, 3)
	d2 := Duration(1, 2, 3)
	d3 := Duration(0, 2, 3)
	if !Equal(d1, d2) {
		t.Fatal("component-equal durations are Equal")
	}
	cmpNone(t, d1, d3)
}

func TestOrderCmpRanksIncomparableTypesAndNullsLast(t *testing.T) {
	if OrderCmp(Int(1), Int(2)) != -1 {
		t.Fatal("Int order")
	}
	if OrderCmp(Bool(true), Int(0)) != -1 {
		t.Fatal("Bool ranks before numbers")
	}
	if OrderCmp(Int(0), Str("a")) != -1 {
		t.Fatal("numbers rank before strings")
	}
	for _, v := range []Value{
		List(nil),
		Node(0),
		Rel(0),
		Path([]uint32{0}, nil),
		Map(nil),
		Temporal(0, LocalDateTime),
		Duration(0, 0, 0),
	} {
		if OrderCmp(v, Null()) != -1 || OrderCmp(Null(), v) != 1 {
			t.Fatalf("null must sort last vs %v", v)
		}
	}
	if OrderCmp(Null(), Null()) != 0 {
		t.Fatal("two nulls are equal")
	}
}

func TestOrderCmpIsTotalOverMixedValues(t *testing.T) {
	vals := []Value{
		Float(math.NaN()),
		Int(3),
		Float(1.5),
		Int(-2),
		Temporal(5, DateTime),
		Temporal(1, Date),
		Str("b"),
		Str("a"),
		Bool(false),
		List([]Value{Int(1)}),
		Null(),
		Float(2.0),
	}
	sort.Slice(vals, func(i, j int) bool { return OrderCmp(vals[i], vals[j]) < 0 })
	if !vals[len(vals)-1].IsNull() {
		t.Fatal("null sorts last")
	}
	sort.Slice(vals, func(i, j int) bool { return OrderCmp(vals[j], vals[i]) < 0 })
	if !vals[0].IsNull() {
		t.Fatal("reverse is also total")
	}
	nums := []Value{Int(3), Float(math.NaN()), Float(1.5)}
	sort.Slice(nums, func(i, j int) bool { return OrderCmp(nums[i], nums[j]) < 0 })
	last, _ := nums[2].AsFloat()
	if !math.IsNaN(last) {
		t.Fatal("NaN sorts after finite numbers within the numeric tier")
	}
}

func TestGroupKeyIntegralFloatMatchesIntAndPaths(t *testing.T) {
	if Key(Float(1.0)) != Key(Int(1)) {
		t.Fatal("1.0 groups with 1")
	}
	if Key(Float(math.Copysign(0, -1))) != Key(Int(0)) {
		t.Fatal("-0.0 groups with 0")
	}
	if !Equal(Int(1), Float(1.0)) {
		t.Fatal("grouping agrees with =")
	}
	if Key(Float(1.5)) == Key(Int(1)) {
		t.Fatal("non-integral float is its own group")
	}
	if Key(Float(math.NaN())) == Key(Float(2.0)) {
		t.Fatal("NaN keys by bit pattern, its own group")
	}
	p1 := Path([]uint32{0, 1}, []uint32{5})
	p2 := Path([]uint32{0, 1}, []uint32{5})
	p3 := Path([]uint32{0, 2}, []uint32{6})
	if !Equal(p1, p2) || Equal(p1, p3) {
		t.Fatal("path equality by (node, rel) sequence")
	}
	if Key(p1) != Key(p2) || Key(p1) == Key(p3) {
		t.Fatal("path group keys agree with =")
	}
}

func TestGroupKeyCoversEveryVariantAndMapIsOrderFree(t *testing.T) {
	values := []Value{
		Null(),
		Bool(true),
		Int(7),
		Float(1.5),
		Str("k"),
		List([]Value{Int(1), Str("x")}),
		Node(2),
		Rel(3),
		Path([]uint32{0, 1}, []uint32{5}),
		Temporal(10, Date),
		Duration(1, 2, 3),
	}
	set := map[string]struct{}{}
	for _, v := range values {
		set[Key(v)] = struct{}{}
	}
	if len(set) != len(values) {
		t.Fatalf("every variant keys distinct: %d != %d", len(set), len(values))
	}
	m1 := Map([]MapEntry{{"a", Int(1)}, {"b", Int(2)}})
	m2 := Map([]MapEntry{{"b", Int(2)}, {"a", Int(1)}})
	if Key(m1) != Key(m2) {
		t.Fatal("equal maps key equal regardless of insertion order")
	}
}

func TestAccessorsReturnNotOKOffType(t *testing.T) {
	if _, ok := Int(1).AsStr(); ok {
		t.Fatal("Int is not Str")
	}
	if _, ok := Null().AsBool(); ok {
		t.Fatal("Null is not Bool")
	}
	if _, ok := Str("x").AsInt(); ok {
		t.Fatal("Str is not Int")
	}
	if _, ok := Int(1).AsNode(); ok {
		t.Fatal("Int is not Node")
	}
	if _, ok := Int(1).AsMap(); ok {
		t.Fatal("Int is not Map")
	}
	if _, _, ok := Int(1).AsPath(); ok {
		t.Fatal("Int is not Path")
	}
	if _, _, _, ok := Int(1).AsDuration(); ok {
		t.Fatal("Int is not Duration")
	}
	if !Null().IsNull() || Int(0).IsNull() {
		t.Fatal("IsNull")
	}
	if Int(0).IsTruthy() || Null().IsTruthy() || !Bool(true).IsTruthy() || Bool(false).IsTruthy() {
		t.Fatal("only Bool(true) is truthy")
	}
	if f, ok := Int(4).AsFloat(); !ok || f != 4.0 {
		t.Fatal("AsFloat coerces Int")
	}
	var zero Value
	if !zero.IsNull() {
		t.Fatal("the zero Value is Null")
	}
}

func TestKleeneTables(t *testing.T) {
	tv := func(v Value) (bool, bool) { return ThreeValued(v) }
	tr, trk := tv(Bool(true))
	fa, fak := tv(Bool(false))
	nu, nuk := tv(Null())
	le, lek := tv(Int(7))
	if !trk || !tr || !fak || fa || nuk || !lek || le {
		t.Fatal("ThreeValued: Bool -> value, Null -> unknown, non-bool -> false")
	}
	and := func(l, lk, r, rk bool) Value { return KleeneAnd(l, lk, r, rk) }
	or := func(l, lk, r, rk bool) Value { return KleeneOr(l, lk, r, rk) }
	if !Equal(and(false, true, nu, nuk), Bool(false)) {
		t.Fatal("false AND null = false")
	}
	if !and(nu, nuk, tr, trk).IsNull() {
		t.Fatal("null AND true = null")
	}
	if !Equal(and(tr, trk, tr, trk), Bool(true)) {
		t.Fatal("true AND true = true")
	}
	if !Equal(or(tr, trk, nu, nuk), Bool(true)) {
		t.Fatal("true OR null = true")
	}
	if !or(nu, nuk, fa, fak).IsNull() {
		t.Fatal("null OR false = null")
	}
	if !Equal(or(fa, fak, fa, fak), Bool(false)) {
		t.Fatal("false OR false = false")
	}
}

func TestOrderCmpSameTierArms(t *testing.T) {
	// Bool, Node, Rel order naturally within their tiers.
	if OrderCmp(Bool(false), Bool(true)) != -1 || OrderCmp(Node(1), Node(2)) != -1 || OrderCmp(Rel(3), Rel(2)) != 1 {
		t.Fatal("bool/node/rel tiers")
	}
	// Lists order element-wise then by length.
	if OrderCmp(List([]Value{Int(1)}), List([]Value{Int(1), Int(0)})) != -1 {
		t.Fatal("shorter list first on equal prefix")
	}
	if OrderCmp(List([]Value{Int(2)}), List([]Value{Int(1), Int(9)})) != 1 {
		t.Fatal("list element order dominates length")
	}
	// Paths order by node sequence, then rel sequence.
	if OrderCmp(Path([]uint32{0, 1}, []uint32{9}), Path([]uint32{0, 2}, []uint32{1})) != -1 {
		t.Fatal("path node order dominates")
	}
	if OrderCmp(Path([]uint32{0, 1}, []uint32{1}), Path([]uint32{0, 1}, []uint32{2})) != -1 {
		t.Fatal("path rel tiebreak")
	}
	// Temporals order by millis then kind discriminant.
	if OrderCmp(Temporal(5, Date), Temporal(5, DateTime)) != -1 || OrderCmp(Temporal(6, Date), Temporal(5, DateTime)) != 1 {
		t.Fatal("temporal millis then kind")
	}
	// Durations order by months, days, millis.
	if OrderCmp(Duration(1, 0, 0), Duration(0, 99, 99)) != 1 || OrderCmp(Duration(1, 2, 3), Duration(1, 2, 4)) != -1 {
		t.Fatal("duration component order")
	}
	// Maps order by key-sorted (key, value) entries, insertion-order-free.
	m1 := Map([]MapEntry{{"b", Int(2)}, {"a", Int(1)}})
	m2 := Map([]MapEntry{{"a", Int(1)}, {"b", Int(2)}})
	if OrderCmp(m1, m2) != 0 {
		t.Fatal("equal maps order equal regardless of insertion order")
	}
	m3 := Map([]MapEntry{{"a", Int(1)}, {"b", Int(9)}})
	if OrderCmp(m1, m3) != -1 {
		t.Fatal("map value order under sorted keys")
	}
	m4 := Map([]MapEntry{{"a", Int(1)}, {"c", Int(2)}})
	if OrderCmp(m1, m4) != -1 {
		t.Fatal("map key order")
	}
	m5 := Map([]MapEntry{{"a", Int(1)}})
	if OrderCmp(m5, m2) != -1 {
		t.Fatal("smaller map first on equal prefix")
	}
	// -0.0 and +0.0 are distinct under the total float order (-0.0 first).
	if OrderCmp(Float(math.Copysign(0, -1)), Float(0)) != -1 {
		t.Fatal("total float order separates -0.0 and +0.0")
	}
}
