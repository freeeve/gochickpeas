// Prop: a resolved single property value whose zero value means absent --
// folding Rust's Option<Prop> + PropExt into one type, so reads chain
// directly: g.Prop(n, "age").I64Or(0).

package chickpeas

// Prop is a property read result. The zero value reads as absent: every
// accessor returns not-ok / its default.
type Prop struct {
	g  *Snapshot
	v  Value
	ok bool
}

// Value returns the raw value; ok is false when the property is absent.
func (p Prop) Value() (Value, bool) {
	return p.v, p.ok
}

// Str returns the string value; ok is false when absent, not a string, or
// empty (dense string columns store a missing value as "", so the empty
// check callers would otherwise repeat is folded in).
func (p Prop) Str() (string, bool) {
	if !p.ok {
		return "", false
	}
	id, isStr := p.v.StrID()
	if !isStr {
		return "", false
	}
	s, found := p.g.atoms.Resolve(id)
	if !found || s == "" {
		return "", false
	}
	return s, true
}

// I64 returns the integer value; ok is false when absent or not an integer.
func (p Prop) I64() (int64, bool) {
	if !p.ok {
		return 0, false
	}
	return p.v.I64()
}

// F64 returns the float value; ok is false when absent or not a float.
func (p Prop) F64() (float64, bool) {
	if !p.ok {
		return 0, false
	}
	return p.v.F64()
}

// Bool returns the boolean value; ok is false when absent or not a boolean.
func (p Prop) Bool() (bool, bool) {
	if !p.ok {
		return false, false
	}
	return p.v.Bool()
}

// StrOr returns the string value, or def when absent / not a string / empty.
func (p Prop) StrOr(def string) string {
	if s, ok := p.Str(); ok {
		return s
	}
	return def
}

// I64Or returns the integer value, or def when absent or not an integer.
func (p Prop) I64Or(def int64) int64 {
	if v, ok := p.I64(); ok {
		return v
	}
	return def
}

// F64Or returns the float value, or def when absent or not a float.
func (p Prop) F64Or(def float64) float64 {
	if v, ok := p.F64(); ok {
		return v
	}
	return def
}

// BoolOr returns the boolean value, or def when absent or not a boolean.
func (p Prop) BoolOr(def bool) bool {
	if v, ok := p.Bool(); ok {
		return v
	}
	return def
}
