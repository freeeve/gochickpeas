// The scalar-function library (FuncOp) and the arithmetic/string-predicate
// kernels, shared verbatim by the interpreter and the compiled path so
// both produce identical results. No scalar function touches the graph
// except startNode/endNode, which resolve in evalScalarFunc.
package eval

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// FuncOp is a resolved scalar function; the compiled path carries this
// instead of the name string so per-row evaluation skips name dispatch.
type FuncOp uint8

// Resolved scalar functions.
const (
	FuncDate FuncOp = iota
	FuncDateTime
	FuncLocalDateTime
	FuncDuration
	FuncLength
	FuncNodes
	FuncRels
	FuncSize
	FuncRange
	FuncLeft
	FuncRight
	FuncSubstring
	FuncID
	FuncAbs
	FuncCeil
	FuncFloor
	FuncRound
	FuncSign
	FuncSqrt
	FuncToFloat
	FuncToInteger
	FuncToString
	FuncToBoolean
	FuncCoalesce
)

// ResolveFuncOp resolves a scalar-function name (case-insensitive); ok is
// false for an unknown function (which yields null at evaluation).
func ResolveFuncOp(name string) (FuncOp, bool) {
	switch strings.ToLower(name) {
	case "date":
		return FuncDate, true
	case "datetime":
		return FuncDateTime, true
	case "localdatetime":
		return FuncLocalDateTime, true
	case "duration":
		return FuncDuration, true
	case "length":
		return FuncLength, true
	case "nodes":
		return FuncNodes, true
	case "rels", "relationships":
		return FuncRels, true
	case "size":
		return FuncSize, true
	case "range":
		return FuncRange, true
	case "left":
		return FuncLeft, true
	case "right":
		return FuncRight, true
	case "substring":
		return FuncSubstring, true
	case "id":
		return FuncID, true
	case "abs":
		return FuncAbs, true
	case "ceil":
		return FuncCeil, true
	case "floor":
		return FuncFloor, true
	case "round":
		return FuncRound, true
	case "sign":
		return FuncSign, true
	case "sqrt":
		return FuncSqrt, true
	case "tofloat":
		return FuncToFloat, true
	case "tointeger":
		return FuncToInteger, true
	case "tostring":
		return FuncToString, true
	case "toboolean":
		return FuncToBoolean, true
	case "coalesce":
		return FuncCoalesce, true
	}
	return 0, false
}

// IsKnownScalarFunc reports whether name is a scalar function the engine
// evaluates (case-insensitive): a ResolveFuncOp op or the graph-resolved
// startNode/endNode. The binder consults this to reject unknown function
// names at plan time rather than silently evaluating them to null.
func IsKnownScalarFunc(name string) bool {
	if _, ok := ResolveFuncOp(name); ok {
		return true
	}
	l := strings.ToLower(name)
	return l == "startnode" || l == "endnode"
}

// evalScalarFunc evaluates a non-aggregate function call. Aggregates never
// reach here -- they are extracted at plan time.
func evalScalarFunc(ctx *Ctx, e *ast.Func, row []value.Value, slots map[string]int) value.Value {
	var argv []value.Value
	if !e.Star {
		argv = make([]value.Value, len(e.Args))
		for i, a := range e.Args {
			argv[i] = Eval(ctx, a, row, slots)
		}
	}
	// startNode(r)/endNode(r) need the graph to resolve a relationship's
	// endpoints from its CSR position, so they resolve here rather than in
	// the graph-less ApplyFunc.
	switch strings.ToLower(e.Name) {
	case "startnode", "endnode":
		if len(argv) > 0 {
			if pos, ok := argv[0].AsRel(); ok {
				if src, dst, ok := ctx.G.RelEndpoints(pos); ok {
					if strings.ToLower(e.Name) == "startnode" {
						return value.Node(src)
					}
					return value.Node(dst)
				}
			}
		}
		return value.Null()
	}
	if op, ok := ResolveFuncOp(e.Name); ok {
		return ApplyFunc(op, argv)
	}
	return value.Null()
}

// ApplyFunc applies a resolved scalar function to its evaluated arguments.
func ApplyFunc(op FuncOp, argv []value.Value) value.Value {
	switch op {
	case FuncDate:
		// date('YYYY-MM-DD') -> a monotonic YYYYMMDD integer (date-typed
		// properties are expected stored the same way; comparisons are
		// integer comparisons).
		if s, ok := arg(argv, 0).AsStr(); ok {
			if v, ok := parseYYYYMMDD(s); ok {
				return value.Int(v)
			}
		}
		return value.Null()
	case FuncDateTime:
		return buildDatetime(arg(argv, 0), value.DateTime)
	case FuncLocalDateTime:
		return buildDatetime(arg(argv, 0), value.LocalDateTime)
	case FuncDuration:
		if m, ok := arg(argv, 0).AsMap(); ok {
			return buildDuration(m)
		}
		return value.Null()
	case FuncID:
		// id(node)/id(rel) -- the internal identity as an integer.
		if n, ok := arg(argv, 0).AsNode(); ok {
			return value.Int(int64(n))
		}
		if p, ok := arg(argv, 0).AsRel(); ok {
			return value.Int(int64(p))
		}
		return value.Null()
	case FuncLength:
		// length(path) -- relationship count.
		if ns, _, ok := arg(argv, 0).AsPath(); ok && len(ns) > 0 {
			return value.Int(int64(len(ns) - 1))
		}
		return value.Null()
	case FuncNodes:
		if ns, _, ok := arg(argv, 0).AsPath(); ok {
			out := make([]value.Value, len(ns))
			for i, n := range ns {
				out[i] = value.Node(n)
			}
			return value.List(out)
		}
		return value.Null()
	case FuncRels:
		// rels(x): a path's relationship list; a var-length rel variable
		// (already a list) as-is; a single rel as a one-element list.
		v := arg(argv, 0)
		if _, rs, ok := v.AsPath(); ok {
			out := make([]value.Value, len(rs))
			for i, r := range rs {
				out[i] = value.Rel(r)
			}
			return value.List(out)
		}
		if _, ok := v.AsList(); ok {
			return v
		}
		if _, ok := v.AsRel(); ok {
			return value.List([]value.Value{v})
		}
		return value.Null()
	case FuncSize:
		// size(list)/size(string) -- element or character count.
		if xs, ok := arg(argv, 0).AsList(); ok {
			return value.Int(int64(len(xs)))
		}
		if s, ok := arg(argv, 0).AsStr(); ok {
			return value.Int(int64(utf8.RuneCountInString(s)))
		}
		return value.Null()
	case FuncRange:
		return applyRange(argv)
	case FuncLeft, FuncRight:
		s, ok1 := arg(argv, 0).AsStr()
		n, ok2 := arg(argv, 1).AsInt()
		if !ok1 || !ok2 || n < 0 {
			return value.Null()
		}
		runes := []rune(s)
		k := min(int(n), len(runes))
		if op == FuncLeft {
			return value.Str(string(runes[:k]))
		}
		return value.Str(string(runes[len(runes)-k:]))
	case FuncSubstring:
		return applySubstring(argv)
	case FuncAbs:
		if i, ok := arg(argv, 0).AsInt(); ok {
			if i == math.MinInt64 {
				return value.Null()
			}
			if i < 0 {
				i = -i
			}
			return value.Int(i)
		}
		if arg(argv, 0).Kind() == value.KindFloat {
			f, _ := arg(argv, 0).AsFloat()
			return value.Float(math.Abs(f))
		}
		return value.Null()
	case FuncCeil, FuncFloor, FuncRound, FuncSqrt:
		x, ok := numArg(argv)
		if !ok {
			return value.Null()
		}
		switch op {
		case FuncCeil:
			return value.Float(math.Ceil(x))
		case FuncFloor:
			return value.Float(math.Floor(x))
		case FuncRound:
			// Half away from zero (Rust f64::round, the openCypher default).
			return value.Float(math.Round(x))
		default:
			return value.Float(math.Sqrt(x))
		}
	case FuncSign:
		x, ok := numArg(argv)
		if !ok {
			return value.Null()
		}
		switch {
		case x > 0:
			return value.Int(1)
		case x < 0:
			return value.Int(-1)
		}
		return value.Int(0)
	case FuncToFloat:
		v := arg(argv, 0)
		if v.Kind() == value.KindInt || v.Kind() == value.KindFloat {
			f, _ := v.AsFloat()
			return value.Float(f)
		}
		if s, ok := v.AsStr(); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return value.Float(f)
			}
		}
		return value.Null()
	case FuncToInteger:
		v := arg(argv, 0)
		if i, ok := v.AsInt(); ok {
			return value.Int(i)
		}
		if v.Kind() == value.KindFloat {
			f, _ := v.AsFloat()
			return value.Int(int64(f))
		}
		if s, ok := v.AsStr(); ok {
			t := strings.TrimSpace(s)
			if i, err := strconv.ParseInt(t, 10, 64); err == nil {
				return value.Int(i)
			}
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				return value.Int(int64(f))
			}
		}
		return value.Null()
	case FuncToString:
		return applyToString(arg(argv, 0))
	case FuncToBoolean:
		v := arg(argv, 0)
		if b, ok := v.AsBool(); ok {
			return value.Bool(b)
		}
		if s, ok := v.AsStr(); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return value.Bool(true)
			case "false":
				return value.Bool(false)
			}
		}
		return value.Null()
	case FuncCoalesce:
		for _, v := range argv {
			if !v.IsNull() {
				return v
			}
		}
		return value.Null()
	}
	return value.Null()
}

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

// applySubstring is substring(s, start[, len]): character-based, start
// past the end yields ""; a null string or negative start/len is null.
func applySubstring(argv []value.Value) value.Value {
	s, ok1 := arg(argv, 0).AsStr()
	start, ok2 := arg(argv, 1).AsInt()
	if !ok1 || !ok2 || start < 0 {
		return value.Null()
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
	}
	return value.Null()
}

// parseYYYYMMDD parses 'YYYY-MM-DD' into the monotonic YYYYMMDD integer.
func parseYYYYMMDD(s string) (int64, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return 0, false
	}
	y, ok1 := parseI64(parts[0])
	m, ok2 := parseI64(parts[1])
	d, ok3 := parseI64(parts[2])
	if !ok1 || !ok2 || !ok3 || m < 1 || m > 12 || d < 1 || d > 31 {
		return 0, false
	}
	return y*10000 + m*100 + d, true
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
			ms := DaysFromCivil(y, uint32(c("month", 1)), uint32(c("day", 1)))*MSPerDay +
				c("hour", 0)*3_600_000 + c("minute", 0)*60_000 + c("second", 0)*1000 + c("millisecond", 0)
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
	return value.Duration(
		get("years")*12+get("months"),
		get("weeks")*7+get("days"),
		get("hours")*3_600_000+get("minutes")*60_000+get("seconds")*1000+get("milliseconds"),
	)
}

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
	// Temporal +/- Duration -> Temporal; + is commutative.
	if (op == ast.OpAdd || op == ast.OpSub) && l.Kind() == value.KindTemporal && r.Kind() == value.KindDuration {
		return applyDur(l, r, op)
	}
	if op == ast.OpAdd && l.Kind() == value.KindDuration && r.Kind() == value.KindTemporal {
		return applyDur(r, l, op)
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

// applyDur applies a duration to a temporal (sign from the operator).
func applyDur(t, d value.Value, op ast.BinOp) value.Value {
	ms, kind, _ := t.AsTemporal()
	months, days, dms, _ := d.AsDuration()
	sign := int64(1)
	if op == ast.OpSub {
		sign = -1
	}
	return value.Temporal(ApplyDuration(ms, months, days, dms, sign), kind)
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
