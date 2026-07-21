// Binary operator evaluation shared by the interpreter and the compiled
// path: string predicates (STARTS WITH / ENDS WITH / CONTAINS) and the
// arithmetic operators (+, -, *, /) over numbers, strings, lists, and
// temporals/durations, with checked integer arithmetic (overflow and
// division by zero yield Null -- eval has no per-row error channel). Split
// from funcs.go, which holds the scalar built-in functions.
package eval

import (
	"math"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// StrPred is a string predicate (STARTS WITH / ENDS WITH / CONTAINS):
// Bool when both operands are strings, Null otherwise. Shared with the
// compiled path.
func StrPred(op ast.BinOp, l, r value.Value) value.Value {
	a, ok1 := l.AsStr()
	b, ok2 := r.AsStr()
	if !ok1 || !ok2 {
		return value.Null()
	}
	switch op {
	case ast.OpStartsWith:
		return value.Bool(strings.HasPrefix(a, b))
	case ast.OpEndsWith:
		return value.Bool(strings.HasSuffix(a, b))
	default: // ast.OpContains
		return value.Bool(strings.Contains(a, b))
	}
}

// Arith is +, -, *, /: string + concatenates; temporal +/- duration is a
// calendar add; Int op Int is checked (overflow and division by zero
// yield Null -- eval has no per-row error channel, so Null is the honest
// result); mixed numerics coerce through float64, where division by zero
// is also Null. Shared with the compiled path.
func Arith(op ast.BinOp, l, r value.Value) value.Value {
	if op == ast.OpAdd && l.Kind() == value.KindStr && r.Kind() == value.KindStr {
		a, _ := l.AsStr()
		b, _ := r.AsStr()
		return value.Str(a + b)
	}
	// List concatenation / append (openCypher `+`, mirroring the Rust
	// engine): list + list chains the elements, list + element appends,
	// element + list prepends. A null operand stays null via the
	// fallthrough.
	if op == ast.OpAdd && !l.IsNull() && !r.IsNull() {
		la, lok := l.AsList()
		rb, rok := r.AsList()
		switch {
		case lok && rok:
			out := make([]value.Value, 0, len(la)+len(rb))
			out = append(out, la...)
			return value.List(append(out, rb...))
		case lok:
			out := make([]value.Value, 0, len(la)+1)
			out = append(out, la...)
			return value.List(append(out, r))
		case rok:
			out := make([]value.Value, 0, len(rb)+1)
			out = append(out, l)
			return value.List(append(out, rb...))
		}
	}
	// Temporal +/- Duration -> Temporal; + is commutative. An Int +/-
	// Duration reads the Int as epoch millis -- the stored form of a
	// temporal column, matching the comparison coercion -- and shifts it,
	// staying an Int (the Rust engine's tasks/151, BI Q17's
	// creationDate + duration(...)).
	if (op == ast.OpAdd || op == ast.OpSub) && r.Kind() == value.KindDuration &&
		(l.Kind() == value.KindTemporal || l.Kind() == value.KindInt) {
		return applyDur(l, r, op)
	}
	if op == ast.OpAdd && l.Kind() == value.KindDuration &&
		(r.Kind() == value.KindTemporal || r.Kind() == value.KindInt) {
		return applyDur(r, l, op)
	}
	// Temporal - Temporal -> Duration (ISO GQL datetime subtraction). Both
	// operands are epoch-millis, so the difference is an exact millisecond
	// duration (months/days stay 0 -- calendar decomposition of a
	// difference is ambiguous and ISO leaves it to DURATION_BETWEEN-style
	// functions).
	if op == ast.OpSub && l.Kind() == value.KindTemporal && r.Kind() == value.KindTemporal {
		a, _, _ := l.AsTemporal()
		b, _, _ := r.AsTemporal()
		if c := a - b; (c < a) == (b > 0) || b == 0 {
			return value.Duration(0, 0, c)
		}
		return value.Null()
	}
	// Duration +/- Duration -> Duration (componentwise, checked).
	if (op == ast.OpAdd || op == ast.OpSub) && l.Kind() == value.KindDuration && r.Kind() == value.KindDuration {
		am, ad, ams, _ := l.AsDuration()
		bm, bd, bms, _ := r.AsDuration()
		m, ok1 := combineInt(op, am, bm)
		d, ok2 := combineInt(op, ad, bd)
		ms, ok3 := combineInt(op, ams, bms)
		if ok1 && ok2 && ok3 {
			return value.Duration(m, d, ms)
		}
		return value.Null()
	}
	// Duration * Int (commutative) and Duration / Int -> Duration,
	// componentwise and exact. Fractional factors are rejected (Null):
	// scaling a calendar month by a fraction has no well-defined carry, so
	// only integral scaling is supported.
	if op == ast.OpMul || op == ast.OpDiv {
		d, k := l, r
		if op == ast.OpMul && l.Kind() == value.KindInt && r.Kind() == value.KindDuration {
			d, k = r, l
		}
		if d.Kind() == value.KindDuration && k.Kind() == value.KindInt {
			kv, _ := k.AsInt()
			if op == ast.OpDiv && kv == 0 {
				return value.Null()
			}
			dm, dd, dms, _ := d.AsDuration()
			m, ok1 := scaleInt(op, dm, kv)
			dy, ok2 := scaleInt(op, dd, kv)
			ms, ok3 := scaleInt(op, dms, kv)
			if ok1 && ok2 && ok3 {
				return value.Duration(m, dy, ms)
			}
			return value.Null()
		}
	}
	if l.Kind() == value.KindInt && r.Kind() == value.KindInt {
		a, _ := l.AsInt()
		b, _ := r.AsInt()
		return checkedInt(op, a, b)
	}
	a, ok1 := l.AsFloat()
	b, ok2 := r.AsFloat()
	if !ok1 || !ok2 {
		return value.Null()
	}
	switch op {
	case ast.OpAdd:
		return value.Float(a + b)
	case ast.OpSub:
		return value.Float(a - b)
	case ast.OpMul:
		return value.Float(a * b)
	default: // ast.OpDiv
		if b == 0.0 {
			return value.Null()
		}
		return value.Float(a / b)
	}
}

// applyDur applies a duration to a temporal (sign from the operator), or
// to an Int read as epoch millis (shifted, staying an Int).
func applyDur(t, d value.Value, op ast.BinOp) value.Value {
	months, days, dms, _ := d.AsDuration()
	sign := int64(1)
	if op == ast.OpSub {
		sign = -1
	}
	if t.Kind() == value.KindInt {
		ms, _ := t.AsInt()
		r, ok := ApplyDuration(ms, months, days, dms, sign)
		if !ok {
			return value.Null()
		}
		return value.Int(r)
	}
	ms, kind, _ := t.AsTemporal()
	r, ok := ApplyDuration(ms, months, days, dms, sign)
	if !ok {
		return value.Null()
	}
	return value.Temporal(r, kind)
}

// combineInt is checked int64 addition/subtraction, comma-ok.
func combineInt(op ast.BinOp, a, b int64) (int64, bool) {
	if op == ast.OpSub {
		c := a - b
		return c, (c < a) == (b > 0) || b == 0
	}
	c := a + b
	return c, (c > a) == (b > 0) || b == 0
}

// scaleInt is checked int64 multiplication/division, comma-ok (the caller
// rejects division by zero before calling).
func scaleInt(op ast.BinOp, c, k int64) (int64, bool) {
	if op == ast.OpDiv {
		if c == math.MinInt64 && k == -1 {
			return 0, false
		}
		return c / k, true
	}
	if c == 0 || k == 0 {
		return 0, true
	}
	if (c == math.MinInt64 && k == -1) || (k == math.MinInt64 && c == -1) {
		return 0, false
	}
	p := c * k
	return p, p/k == c
}

// checkedInt is checked 64-bit integer arithmetic: overflow (including
// MinInt64 negation cases) and division by zero yield Null rather than a
// wrapped wrong value.
func checkedInt(op ast.BinOp, a, b int64) value.Value {
	switch op {
	case ast.OpAdd:
		c := a + b
		if (c > a) == (b > 0) || b == 0 {
			return value.Int(c)
		}
	case ast.OpSub:
		c := a - b
		if (c < a) == (b > 0) || b == 0 {
			return value.Int(c)
		}
	case ast.OpMul:
		if a == 0 || b == 0 {
			return value.Int(0)
		}
		if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
			return value.Null()
		}
		c := a * b
		if c/b == a {
			return value.Int(c)
		}
	default: // ast.OpDiv
		if b == 0 || (a == math.MinInt64 && b == -1) {
			return value.Null()
		}
		return value.Int(a / b)
	}
	return value.Null()
}
