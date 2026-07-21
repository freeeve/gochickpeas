// Scalar built-in application helpers: positional argument access, the
// range/substring/toString value builders, and the datetime/duration
// constructors with their checked integer arithmetic. Split from funcs.go,
// which holds the function registry (ResolveFuncOp) and the ApplyFunc
// dispatch.
package eval

import (
	"math"
	"strconv"

	"github.com/freeeve/gochickpeas/gql/value"
)

// arg reads argv[i], Null when absent.
func arg(argv []value.Value, i int) value.Value {
	if i < len(argv) {
		return argv[i]
	}
	return value.Null()
}

// numArg is the single numeric argument of a math function as float64
// (Int or Float), comma-ok.
func numArg(argv []value.Value) (float64, bool) {
	return arg(argv, 0).AsFloat()
}

// applyRange is range(start, end[, step]): the inclusive integer sequence
// (step defaults to 1; a zero step is null).
func applyRange(argv []value.Value) value.Value {
	start, ok1 := arg(argv, 0).AsInt()
	end, ok2 := arg(argv, 1).AsInt()
	if !ok1 || !ok2 {
		return value.Null()
	}
	step := int64(1)
	if len(argv) >= 3 {
		if s, ok := arg(argv, 2).AsInt(); ok {
			step = s
		}
	}
	if step == 0 {
		return value.Null()
	}
	var out []value.Value
	for x := start; (step > 0 && x <= end) || (step < 0 && x >= end); x += step {
		out = append(out, value.Int(x))
	}
	return value.List(out)
}

// asciiOnly reports whether s holds only single-byte runes, so character
// indexing equals byte indexing.
func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// applySubstring is substring(s, start[, len]): character-based, start
// past the end yields ""; a null string or negative start/len is null.
// An all-ASCII source -- the overwhelmingly common case -- slices the
// input string directly (a Go substring shares the backing), skipping the
// rune conversion and the fresh string a hot aggregate argument would
// otherwise pay per row.
func applySubstring(argv []value.Value) value.Value {
	s, ok1 := arg(argv, 0).AsStr()
	start, ok2 := arg(argv, 1).AsInt()
	if !ok1 || !ok2 || start < 0 {
		return value.Null()
	}
	if asciiOnly(s) {
		lo := min(int(start), len(s))
		if len(argv) >= 3 {
			n, ok := arg(argv, 2).AsInt()
			if !ok || n < 0 {
				return value.Null()
			}
			hi := min(lo+int(n), len(s))
			return value.Str(s[lo:hi])
		}
		return value.Str(s[lo:])
	}
	runes := []rune(s)
	lo := min(int(start), len(runes))
	if len(argv) >= 3 {
		n, ok := arg(argv, 2).AsInt()
		if !ok || n < 0 {
			return value.Null()
		}
		hi := min(lo+int(n), len(runes))
		return value.Str(string(runes[lo:hi]))
	}
	return value.Str(string(runes[lo:]))
}

// applyToString renders a scalar as a string: an integral Float keeps a
// trailing .0 (openCypher prints 1.0, not 1); inf/NaN render as the Rust
// engine prints them so cross-engine goldens agree.
func applyToString(v value.Value) value.Value {
	switch v.Kind() {
	case value.KindInt:
		i, _ := v.AsInt()
		return value.Str(strconv.FormatInt(i, 10))
	case value.KindFloat:
		f, _ := v.AsFloat()
		switch {
		case math.IsNaN(f):
			return value.Str("NaN")
		case math.IsInf(f, 1):
			return value.Str("inf")
		case math.IsInf(f, -1):
			return value.Str("-inf")
		case f == math.Trunc(f):
			return value.Str(strconv.FormatFloat(f, 'f', 1, 64))
		}
		return value.Str(strconv.FormatFloat(f, 'f', -1, 64))
	case value.KindBool:
		b, _ := v.AsBool()
		return value.Str(strconv.FormatBool(b))
	case value.KindStr:
		return v
	case value.KindTemporal:
		ms, kind, _ := v.AsTemporal()
		return value.Str(ISOString(ms, kind))
	}
	// A duration has no single established string form here (the wasm/Python
	// surfaces emit the {months, days, millis} component form, not a string),
	// so toString(duration) stays Null rather than bake an arbitrary choice.
	return value.Null()
}

// buildDatetime builds a temporal from a datetime(...) argument: an ISO
// string, an i64 epoch-millis, another temporal (kind changes), an
// {epochMillis: n} map, or a {year, month, day, hour, ...} component map.
func buildDatetime(v value.Value, kind value.TemporalKind) value.Value {
	switch v.Kind() {
	case value.KindStr:
		s, _ := v.AsStr()
		if ms, ok := ParseISO(s); ok {
			return value.Temporal(ms, kind)
		}
	case value.KindInt:
		ms, _ := v.AsInt()
		return value.Temporal(ms, kind)
	case value.KindTemporal:
		ms, _, _ := v.AsTemporal()
		return value.Temporal(ms, kind)
	case value.KindMap:
		m, _ := v.AsMap()
		get := func(k string) (int64, bool) { return mapGet(m, k).AsInt() }
		if ms, ok := get("epochMillis"); ok {
			return value.Temporal(ms, kind)
		}
		if y, ok := get("year"); ok {
			c := func(k string, dflt int64) int64 {
				if x, ok := get(k); ok {
					return x
				}
				return dflt
			}
			msOfDay, ok := sumScaled(
				[2]int64{c("hour", 0), 3_600_000}, [2]int64{c("minute", 0), 60_000},
				[2]int64{c("second", 0), 1000}, [2]int64{c("millisecond", 0), 1})
			if !ok {
				return value.Null()
			}
			ms, ok := civilMillis(y, uint32(c("month", 1)), uint32(c("day", 1)), msOfDay)
			if !ok {
				return value.Null()
			}
			return value.Temporal(ms, kind)
		}
	}
	return value.Null()
}

// buildDuration builds a duration from a duration({years, months, weeks,
// days, hours, minutes, seconds, milliseconds}) map.
func buildDuration(m []value.MapEntry) value.Value {
	get := func(k string) int64 {
		if v, ok := mapGet(m, k).AsInt(); ok {
			return v
		}
		return 0
	}
	// Unit conversions overflow at build time, before any temporal is
	// touched; a wrapped duration would silently corrupt every later add.
	months, ok1 := addMul(get("years"), 12, get("months"))
	days, ok2 := addMul(get("weeks"), 7, get("days"))
	ms, ok3 := sumScaled(
		[2]int64{get("hours"), 3_600_000}, [2]int64{get("minutes"), 60_000},
		[2]int64{get("seconds"), 1000}, [2]int64{get("milliseconds"), 1})
	if !ok1 || !ok2 || !ok3 {
		return value.Null()
	}
	return value.Duration(months, days, ms)
}

// addMul is checked a*k + b, comma-ok.
func addMul(a, k, b int64) (int64, bool) {
	p, ok := mulChk(a, k)
	if !ok {
		return 0, false
	}
	return addChk(p, b)
}

// sumScaled is the checked sum of value*scale terms, comma-ok.
func sumScaled(terms ...[2]int64) (int64, bool) {
	var sum int64
	for _, t := range terms {
		p, ok := mulChk(t[0], t[1])
		if !ok {
			return 0, false
		}
		if sum, ok = addChk(sum, p); !ok {
			return 0, false
		}
	}
	return sum, true
}
