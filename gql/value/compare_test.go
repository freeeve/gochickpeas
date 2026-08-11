package value

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestTotalOrderF64 covers the IEEE-754 total order exposed for typed sort
// keys: finite ordering, the -0 < +0 distinction (which a normal compare
// collapses), and a definite, antisymmetric position for NaN.
func TestTotalOrderF64(t *testing.T) {
	if TotalOrderF64(1.0, 2.0) >= 0 || TotalOrderF64(2.0, 1.0) <= 0 || TotalOrderF64(1.5, 1.5) != 0 {
		t.Fatal("finite ordering broken")
	}
	// The distinguishing feature vs a normal compare: negative zero sorts
	// below positive zero.
	negZero := math.Copysign(0, -1)
	if TotalOrderF64(negZero, 0.0) >= 0 || TotalOrderF64(0.0, negZero) <= 0 {
		t.Fatal("-0 must sort below +0 in the total order")
	}
	// NaN has a definite, antisymmetric position (unlike a normal compare,
	// which reports NaN as unordered).
	nan := math.NaN()
	c1, c2 := TotalOrderF64(nan, 1.0), TotalOrderF64(1.0, nan)
	if c1 == 0 || c2 == 0 || (c1 > 0) == (c2 > 0) {
		t.Fatalf("NaN must be ordered antisymmetrically: %d / %d", c1, c2)
	}
}

// TestOrderCmpTotalOrderLaws asserts antisymmetry and transitivity of
// OrderCmp exhaustively over a mixed sample including NaN and both
// infinities (ragedb's 294 lesson: a NaN-broken comparator passes every
// PAIRWISE check -- cmp(NaN,1)=0 and cmp(NaN,2)=0 each look plausible;
// only the triple over 1, 2, NaN exposes the intransitivity, and a
// comparator that is not a total order is undefined behavior under a
// sort). NaN needs no literal to arrive: overflow arithmetic produces it
// (1e308*10 -> +Inf; Inf - Inf -> NaN), covered by the derived entries.
func TestOrderCmpTotalOrderLaws(t *testing.T) {
	// The overflow route must use runtime variables: Go CONSTANT
	// arithmetic evaluates in arbitrary precision, so (1e308*10)-(1e308*10)
	// is exactly 0 at compile time -- the fixture itself would hide the
	// NaN it exists to produce.
	big := 1e308
	inf := big * 10 // overflows to +Inf at runtime
	overflowNaN := inf - inf
	sample := []Value{
		Null(),
		Bool(false), Bool(true),
		Int(math.MinInt64), Int(-1), Int(0), Int(1), Int(math.MaxInt64),
		Float(math.Inf(-1)), Float(-1.5), Float(math.Copysign(0, -1)),
		Float(0), Float(1.5), Float(inf), Float(math.NaN()), Float(overflowNaN),
		Str(""), Str("a"), Str("b"),
		List(nil), List([]Value{Int(1)}), List([]Value{Float(math.NaN())}),
	}
	sgn := func(c int) int {
		if c > 0 {
			return 1
		}
		if c < 0 {
			return -1
		}
		return 0
	}
	for i, a := range sample {
		if OrderCmp(a, a) != 0 {
			t.Fatalf("sample[%d] not equal to itself", i)
		}
		for j, b := range sample {
			if sgn(OrderCmp(a, b)) != -sgn(OrderCmp(b, a)) {
				t.Fatalf("antisymmetry broken at sample[%d], sample[%d]", i, j)
			}
		}
	}
	for i, a := range sample {
		for j, b := range sample {
			for k, c := range sample {
				ab, bc, ac := OrderCmp(a, b), OrderCmp(b, c), OrderCmp(a, c)
				if ab <= 0 && bc <= 0 && ac > 0 {
					t.Fatalf("transitivity broken over sample[%d,%d,%d]: %d %d %d", i, j, k, ab, bc, ac)
				}
				if ab == 0 && bc == 0 && ac != 0 {
					t.Fatalf("equivalence not transitive over sample[%d,%d,%d]", i, j, k)
				}
			}
		}
	}
	// Control: the SEMANTIC comparator keeps NaN incomparable (the SQL
	// three-valued relation), never equal-to-everything -- the two
	// comparators answer different questions and must stay distinct.
	if _, ok := Compare(Float(overflowNaN), Float(1)); ok {
		t.Fatal("Compare must report NaN incomparable, not ordered")
	}
	if c, ok := Compare(Int(1), Int(2)); !ok || c >= 0 {
		t.Fatal("ordinary Compare behavior disturbed")
	}
}

// TestOrderNumF64 covers the numeric-tier encoding: an Int/Float yields its
// float value, anything else yields NaN, and it pairs with TotalOrderF64.
func TestOrderNumF64(t *testing.T) {
	if OrderNumF64(Int(5)) != 5.0 || OrderNumF64(Float(2.5)) != 2.5 {
		t.Fatal("numeric values must encode to their float")
	}
	if !math.IsNaN(OrderNumF64(Str("x"))) || !math.IsNaN(OrderNumF64(Null())) {
		t.Fatal("non-numeric values must encode to NaN")
	}
	if TotalOrderF64(OrderNumF64(Int(1)), OrderNumF64(Int(2))) >= 0 {
		t.Fatal("Int(1) must order below Int(2) through the paired kernels")
	}
}

// TestSameBacking covers the O(1) payload-identity check: a value shares
// backing with itself, separately-built equal lists do not, and equal
// scalars (no ext) do; a true result implies Identical.
func TestSameBacking(t *testing.T) {
	l := List([]Value{Int(1), Int(2)})
	if !SameBacking(l, l) {
		t.Fatal("a value must share backing with itself")
	}
	if SameBacking(l, List([]Value{Int(1), Int(2)})) {
		t.Fatal("distinct list allocations do not share backing")
	}
	if !SameBacking(Int(3), Int(3)) || SameBacking(Int(3), Int(4)) {
		t.Fatal("scalar backing is by kind+payload")
	}
	// The documented guarantee: SameBacking implies Identical.
	if SameBacking(l, l) && !Identical(l, l) {
		t.Fatal("SameBacking must imply Identical")
	}
}

// TestIdentical covers bit-exact identity: Null==Null, no numeric coercion,
// floats by bit pattern (so NaN is self-identical and -0 != +0), strings by
// content, and lists element-wise.
func TestIdentical(t *testing.T) {
	if !Identical(Null(), Null()) {
		t.Fatal("null is identical to null")
	}
	if !Identical(Int(3), Int(3)) || Identical(Int(3), Int(4)) {
		t.Fatal("int identity by value")
	}
	// No numeric coercion, unlike Equal.
	if Identical(Int(3), Float(3.0)) {
		t.Fatal("Identical must not coerce int to float")
	}
	if !Identical(Str("a"), Str("a")) || Identical(Str("a"), Str("b")) {
		t.Fatal("string identity by content")
	}
	// Floats by bit pattern: NaN is self-identical (Equal would say no), and
	// -0 differs from +0.
	if !Identical(Float(math.NaN()), Float(math.NaN())) {
		t.Fatal("Identical compares floats by bit pattern (NaN self-identical)")
	}
	if Identical(Float(math.Copysign(0, -1)), Float(0.0)) {
		t.Fatal("-0 and +0 differ by bit pattern")
	}
	// Lists recurse element-wise; length and content both matter.
	if !Identical(List([]Value{Int(1), Int(2)}), List([]Value{Int(1), Int(2)})) {
		t.Fatal("equal lists are identical")
	}
	if Identical(List([]Value{Int(1)}), List([]Value{Int(2)})) ||
		Identical(List([]Value{Int(1)}), List([]Value{Int(1), Int(2)})) {
		t.Fatal("differing content or length breaks list identity")
	}

	// The remaining ext-carrying kinds compare by their payloads.
	if !Identical(Temporal(1000, Date), Temporal(1000, Date)) || Identical(Temporal(1000, Date), Temporal(2000, Date)) {
		t.Fatal("temporal identity by millis+kind")
	}
	if !Identical(Duration(1, 2, 3), Duration(1, 2, 3)) || Identical(Duration(1, 2, 3), Duration(1, 2, 4)) {
		t.Fatal("duration identity by months/days/millis")
	}
	if !Identical(Map([]MapEntry{{Key: "a", Val: Int(1)}}), Map([]MapEntry{{Key: "a", Val: Int(1)}})) ||
		Identical(Map([]MapEntry{{Key: "a", Val: Int(1)}}), Map([]MapEntry{{Key: "a", Val: Int(2)}})) ||
		Identical(Map([]MapEntry{{Key: "a", Val: Int(1)}}), Map(nil)) {
		t.Fatal("map identity by entry key+value and length")
	}
	if !Identical(Path([]chickpeas.NodeID{0, 1}, []uint32{5}), Path([]chickpeas.NodeID{0, 1}, []uint32{5})) ||
		Identical(Path([]chickpeas.NodeID{0, 1}, []uint32{5}), Path([]chickpeas.NodeID{0, 2}, []uint32{5})) {
		t.Fatal("path identity by node and rel sequences")
	}
}
