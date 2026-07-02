package chickpeas

import "math"

// ValueKind discriminates a Value.
type ValueKind uint8

const (
	// KindStr is an interned-string value (the payload is an atom id).
	KindStr ValueKind = iota
	// KindI64 is an integer value.
	KindI64
	// KindF64 is a float value, stored by bit pattern.
	KindF64
	// KindBool is a boolean value.
	KindBool
)

// Value is a property value: the Go port of the Rust ValueId tagged union.
// It is comparable -- usable as a map key with == -- with the same equality
// semantics as the Rust derive: strings compare by atom id and floats by
// IEEE-754 bit pattern (so NaN == NaN when the payloads match, and
// 0.0 != -0.0).
type Value struct {
	kind ValueKind
	bits uint64
}

// StrValue is an interned-string value holding an atom id.
func StrValue(atom uint32) Value {
	return Value{kind: KindStr, bits: uint64(atom)}
}

// I64Value is an integer value.
func I64Value(v int64) Value {
	return Value{kind: KindI64, bits: uint64(v)}
}

// F64Value is a float value, stored by bit pattern.
func F64Value(v float64) Value {
	return Value{kind: KindF64, bits: math.Float64bits(v)}
}

// BoolValue is a boolean value.
func BoolValue(v bool) Value {
	var b uint64
	if v {
		b = 1
	}
	return Value{kind: KindBool, bits: b}
}

// Kind reports the value's discriminant.
func (v Value) Kind() ValueKind { return v.kind }

// StrID returns the interned-string atom id; ok is false for other kinds.
func (v Value) StrID() (uint32, bool) {
	return uint32(v.bits), v.kind == KindStr
}

// I64 returns the integer value; ok is false for other kinds.
func (v Value) I64() (int64, bool) {
	return int64(v.bits), v.kind == KindI64
}

// F64 returns the float value; ok is false for other kinds.
func (v Value) F64() (float64, bool) {
	return math.Float64frombits(v.bits), v.kind == KindF64
}

// Bool returns the boolean value; ok is false for other kinds.
func (v Value) Bool() (bool, bool) {
	return v.bits != 0, v.kind == KindBool
}
