package chickpeas_test

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func TestValueAccessors(t *testing.T) {
	if id, ok := chickpeas.StrValue(7).StrID(); !ok || id != 7 {
		t.Fatal("str accessor")
	}
	if v, ok := chickpeas.I64Value(-42).I64(); !ok || v != -42 {
		t.Fatal("i64 accessor")
	}
	if v, ok := chickpeas.F64Value(2.5).F64(); !ok || v != 2.5 {
		t.Fatal("f64 accessor")
	}
	if v, ok := chickpeas.BoolValue(true).Bool(); !ok || !v {
		t.Fatal("bool accessor")
	}
	// Mistyped reads are not-ok.
	if _, ok := chickpeas.I64Value(1).F64(); ok {
		t.Fatal("i64 read as f64")
	}
	if _, ok := chickpeas.StrValue(0).Bool(); ok {
		t.Fatal("str read as bool")
	}
}

func TestValueEqualitySemantics(t *testing.T) {
	// Same kind + payload compare equal; kinds never cross-compare equal
	// even with identical bits.
	if chickpeas.I64Value(1) == chickpeas.BoolValue(true) {
		t.Fatal("i64(1) == bool(true)")
	}
	if chickpeas.StrValue(1) == chickpeas.I64Value(1) {
		t.Fatal("str(1) == i64(1)")
	}
	// F64 compares by bit pattern: NaN == NaN (same payload), 0.0 != -0.0.
	nan := chickpeas.F64Value(math.NaN())
	if nan != chickpeas.F64Value(math.NaN()) {
		t.Fatal("identical NaN payloads compare unequal")
	}
	if chickpeas.F64Value(0.0) == chickpeas.F64Value(math.Copysign(0, -1)) {
		t.Fatal("0.0 == -0.0 under bit comparison")
	}
	// Usable as a map key.
	m := map[chickpeas.Value]int{
		nan:                        1,
		chickpeas.I64Value(5):      2,
		chickpeas.StrValue(5):      3,
		chickpeas.BoolValue(false): 4,
	}
	if m[chickpeas.F64Value(math.NaN())] != 1 || m[chickpeas.I64Value(5)] != 2 ||
		m[chickpeas.StrValue(5)] != 3 || m[chickpeas.BoolValue(false)] != 4 {
		t.Fatal("map-key lookups wrong")
	}
}
