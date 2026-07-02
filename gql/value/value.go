// Package value holds the GQL engine's runtime values -- what the executor
// produces and expressions evaluate to. Distinct from the engine's columnar
// chickpeas.Value (a 4-kind stored scalar): a runtime value can also be a
// list, map, path, temporal, or a bound node/rel handle. Port of the Rust
// cypher crate's Value<'g>; Go strings are immutable shared references, so
// the 'g borrow lifetime disappears.
package value

import (
	"math"

	chickpeas "github.com/freeeve/gochickpeas"
)

// Kind discriminates a Value.
type Kind uint8

const (
	// KindNull is the absent/unknown value; the zero Value is Null.
	KindNull Kind = iota
	// KindBool is a boolean value.
	KindBool
	// KindInt is a 64-bit integer value.
	KindInt
	// KindFloat is a 64-bit float value.
	KindFloat
	// KindStr is a string value.
	KindStr
	// KindList is a list value, e.g. the right-hand side of IN [...].
	KindList
	// KindNode is a bound graph node -- RETURN n yields this.
	KindNode
	// KindRel is a bound relationship, identified by its outgoing-CSR
	// position (a chickpeas.RelRef.Pos).
	KindRel
	// KindPath is a path: the node sequence plus the CSR positions of the
	// relationships between consecutive nodes (len(rels) == len(nodes)-1).
	KindPath
	// KindMap is a map value with entries in insertion order; equality is
	// by content regardless of order.
	KindMap
	// KindTemporal is a date/datetime/localdatetime, epoch-millis-backed.
	KindTemporal
	// KindDuration is a duration by calendar components (months, days,
	// millis); equatable but not totally ordered.
	KindDuration
)

// TemporalKind says which temporal a KindTemporal value is. All are epoch
// milliseconds (UTC) under the hood; the kind only affects display.
// Date is midnight UTC of the day.
type TemporalKind uint8

const (
	// Date is a calendar date (midnight UTC).
	Date TemporalKind = iota
	// DateTime is a zoned datetime (treated as UTC).
	DateTime
	// LocalDateTime is a wall-clock datetime.
	LocalDateTime
)

// MapEntry is one key/value pair of a map value, in insertion order.
type MapEntry struct {
	Key string
	Val Value
}

// ext carries the payloads that don't fit the inline word: list elements,
// map entries, path sequences, and a duration's calendar components.
type ext struct {
	list    []Value
	entries []MapEntry
	nodes   []chickpeas.NodeID
	rels    []uint32
	months  int64
	days    int64
}

// Value is one runtime value. The zero Value is Null. Scalars store their
// payload inline (num holds the int64 bits, float bits, bool, node id, rel
// position, or temporal millis); strings ride in str; lists, maps, paths,
// and durations share the ext pointer.
type Value struct {
	kind Kind
	aux  uint8
	num  uint64
	str  string
	ext  *ext
}

// Null is the absent/unknown value (same as the zero Value).
func Null() Value { return Value{} }

// Bool is a boolean value.
func Bool(b bool) Value {
	var n uint64
	if b {
		n = 1
	}
	return Value{kind: KindBool, num: n}
}

// Int is an integer value.
func Int(i int64) Value { return Value{kind: KindInt, num: uint64(i)} }

// Float is a float value.
func Float(f float64) Value { return Value{kind: KindFloat, num: math.Float64bits(f)} }

// Str is a string value. The string is referenced, not copied -- a property
// read from the immutable snapshot shares the interner's text.
func Str(s string) Value { return Value{kind: KindStr, str: s} }

// List is a list value; the slice is taken by reference, not copied.
func List(vs []Value) Value { return Value{kind: KindList, ext: &ext{list: vs}} }

// Node is a bound graph node.
func Node(id chickpeas.NodeID) Value { return Value{kind: KindNode, num: uint64(id)} }

// Rel is a bound relationship by outgoing-CSR position.
func Rel(pos uint32) Value { return Value{kind: KindRel, num: uint64(pos)} }

// Path is a path value over a node sequence and the relationship positions
// between consecutive nodes; both slices are taken by reference.
func Path(nodes []chickpeas.NodeID, rels []uint32) Value {
	return Value{kind: KindPath, ext: &ext{nodes: nodes, rels: rels}}
}

// Map is a map value; entries keep insertion order and are taken by
// reference.
func Map(entries []MapEntry) Value { return Value{kind: KindMap, ext: &ext{entries: entries}} }

// Temporal is an epoch-millis temporal of the given kind.
func Temporal(millis int64, k TemporalKind) Value {
	return Value{kind: KindTemporal, aux: uint8(k), num: uint64(millis)}
}

// Duration is a duration by calendar components.
func Duration(months, days, millis int64) Value {
	return Value{kind: KindDuration, num: uint64(millis), ext: &ext{months: months, days: days}}
}

// Kind reports the value's discriminant.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether the value is Null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// IsTruthy is the truthiness WHERE and the boolean operators use: only
// Bool(true) is true; Null and non-booleans are not-true.
func (v Value) IsTruthy() bool { return v.kind == KindBool && v.num != 0 }

// AsBool returns the boolean payload; ok is false for other kinds.
func (v Value) AsBool() (bool, bool) { return v.num != 0, v.kind == KindBool }

// AsInt returns the integer payload; ok is false for other kinds.
func (v Value) AsInt() (int64, bool) { return int64(v.num), v.kind == KindInt }

// AsFloat returns the numeric payload as a float, coercing an integer; ok
// is false for non-numeric kinds.
func (v Value) AsFloat() (float64, bool) { return v.asNum() }

// AsStr returns the string payload; ok is false for other kinds.
func (v Value) AsStr() (string, bool) { return v.str, v.kind == KindStr }

// AsList returns the list elements; ok is false for other kinds.
func (v Value) AsList() ([]Value, bool) {
	if v.kind != KindList {
		return nil, false
	}
	return v.ext.list, true
}

// AsNode returns the bound node id; ok is false for other kinds.
func (v Value) AsNode() (chickpeas.NodeID, bool) {
	return chickpeas.NodeID(v.num), v.kind == KindNode
}

// AsRel returns the bound relationship's CSR position; ok is false for
// other kinds.
func (v Value) AsRel() (uint32, bool) { return uint32(v.num), v.kind == KindRel }

// AsPath returns the path's node sequence and relationship positions; ok is
// false for other kinds.
func (v Value) AsPath() (nodes []chickpeas.NodeID, rels []uint32, ok bool) {
	if v.kind != KindPath {
		return nil, nil, false
	}
	return v.ext.nodes, v.ext.rels, true
}

// AsMap returns the map entries in insertion order; ok is false for other
// kinds.
func (v Value) AsMap() ([]MapEntry, bool) {
	if v.kind != KindMap {
		return nil, false
	}
	return v.ext.entries, true
}

// AsTemporal returns the temporal's epoch millis and kind; ok is false for
// other kinds.
func (v Value) AsTemporal() (millis int64, k TemporalKind, ok bool) {
	return int64(v.num), TemporalKind(v.aux), v.kind == KindTemporal
}

// AsDuration returns the duration's components; ok is false for other kinds.
func (v Value) AsDuration() (months, days, millis int64, ok bool) {
	if v.kind != KindDuration {
		return 0, 0, 0, false
	}
	return v.ext.months, v.ext.days, int64(v.num), true
}

// asNum is the shared numeric coercion: Int and Float read as float64,
// everything else is non-numeric.
func (v Value) asNum() (float64, bool) {
	switch v.kind {
	case KindInt:
		return float64(int64(v.num)), true
	case KindFloat:
		return math.Float64frombits(v.num), true
	default:
		return 0, false
	}
}
