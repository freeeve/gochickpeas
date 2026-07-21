// The scalar-function library (FuncOp) and the arithmetic/string-predicate
// kernels, shared verbatim by the interpreter and the compiled path so
// both produce identical results. No scalar function touches the graph
// except startNode/endNode, which resolve in evalScalarFunc.
package eval

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/unorm"
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
	FuncLower
	FuncUpper
	FuncCharLength
	FuncCardinality
	FuncTrim
	FuncLTrim
	FuncRTrim
	FuncMod
	FuncPower
	FuncExp
	FuncLn
	FuncLog10
	FuncSin
	FuncCos
	FuncTan
	FuncAsin
	FuncAcos
	FuncAtan
	FuncDegrees
	FuncRadians
	FuncNullIf
	FuncHead
	FuncLast
	FuncTail
	FuncElementID
	FuncNormalize
	FuncIsNormalized
)

// ResolveFuncOp resolves a scalar-function name (case-insensitive); ok is
// false for an unknown function (which yields null at evaluation).
func ResolveFuncOp(name string) (FuncOp, bool) {
	switch strings.ToLower(name) {
	case "date":
		return FuncDate, true
	case "datetime", "zoned_datetime":
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
	case "lower", "tolower":
		return FuncLower, true
	case "upper", "toupper":
		return FuncUpper, true
	case "char_length", "character_length":
		return FuncCharLength, true
	case "cardinality":
		return FuncCardinality, true
	case "trim":
		return FuncTrim, true
	case "normalize":
		return FuncNormalize, true
	case "is_normalized":
		return FuncIsNormalized, true
	case "ltrim":
		return FuncLTrim, true
	case "rtrim":
		return FuncRTrim, true
	case "mod":
		return FuncMod, true
	case "power", "pow":
		return FuncPower, true
	case "exp":
		return FuncExp, true
	case "ln":
		return FuncLn, true
	case "log10":
		return FuncLog10, true
	case "sin":
		return FuncSin, true
	case "cos":
		return FuncCos, true
	case "tan":
		return FuncTan, true
	case "asin":
		return FuncAsin, true
	case "acos":
		return FuncAcos, true
	case "atan":
		return FuncAtan, true
	case "degrees":
		return FuncDegrees, true
	case "radians":
		return FuncRadians, true
	case "nullif":
		return FuncNullIf, true
	case "head":
		return FuncHead, true
	case "last":
		return FuncLast, true
	case "tail":
		return FuncTail, true
	case "element_id", "elementid":
		return FuncElementID, true
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
	return l == "startnode" || l == "endnode" || l == "type" || l == "labels"
}

// evalScalarFunc evaluates a non-aggregate function call. Aggregates never
// reach here -- they are extracted at plan time. An all-literal call is
// memoized on the Ctx (see constCalls) so hot correlated evaluation --
// subquery WHEREs, list-predicate bodies -- pays its construction once per
// execution.
func evalScalarFunc(ctx *Ctx, e *ast.Func, row []value.Value, slots map[string]int) value.Value {
	if p, seen := ctx.constCalls[e]; seen && p != nil {
		return *p
	} else if !seen {
		constant := !e.Star
		for _, a := range e.Args {
			if _, isLit := a.(*ast.Lit); !isLit {
				constant = false
				break
			}
		}
		if ctx.constCalls == nil {
			ctx.constCalls = map[*ast.Func]*value.Value{}
		}
		if constant {
			v := evalScalarFuncUncached(ctx, e, row, slots)
			ctx.constCalls[e] = &v
			return v
		}
		ctx.constCalls[e] = nil
	}
	return evalScalarFuncUncached(ctx, e, row, slots)
}

func evalScalarFuncUncached(ctx *Ctx, e *ast.Func, row []value.Value, slots map[string]int) value.Value {
	// The argument row is a frame on the Ctx's argv stack rather than a
	// fresh slice per call: a nested call inside an argument pushes and
	// pops a deeper frame before this one continues filling, and no
	// ApplyFunc arm retains the argv slice (arms read elements by value or
	// build fresh output), so popping on return is safe. AST nodes are
	// shared across concurrent cached runs; the stack lives on the
	// per-execution Ctx, never the node.
	var argv []value.Value
	off := len(ctx.argvStack)
	if !e.Star {
		ctx.argvStack = slices.Grow(ctx.argvStack, len(e.Args))[:off+len(e.Args)]
		argv = ctx.argvStack[off : off+len(e.Args)]
		for i, a := range e.Args {
			argv[i] = Eval(ctx, a, row, slots)
		}
	}
	v := applyScalarFunc(ctx, e, argv)
	ctx.argvStack = ctx.argvStack[:off]
	return v
}

func applyScalarFunc(ctx *Ctx, e *ast.Func, argv []value.Value) value.Value {
	// startNode(r)/endNode(r)/type(r) need the graph to resolve a
	// relationship from its CSR position, so they resolve here rather than in
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
	case "type":
		if len(argv) > 0 {
			if pos, ok := argv[0].AsRel(); ok {
				if name, ok := ctx.G.RelTypeAt(pos); ok {
					return value.Str(name)
				}
			}
		}
		return value.Null()
	case "labels":
		// Composed from the label registry + membership: label counts are
		// small, so the per-call sweep beats adding a per-node enumeration
		// to the graph seam.
		if len(argv) > 0 {
			if id, ok := argv[0].AsNode(); ok {
				var out []value.Value
				for _, l := range ctx.G.LabelNames() {
					if ctx.G.HasLabel(id, l) {
						out = append(out, value.Str(l))
					}
				}
				return value.List(out)
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
		// date(x) -> Temporal(Date): an ISO string, an i64 epoch-millis,
		// another temporal, or a component map, truncated to midnight UTC.
		// Component accessors and ordering come with the temporal kind; the
		// retired YYYYMMDD-integer form read .year as an epoch and nulled
		// the int/temporal arguments BI Q16 itself uses.
		if t := buildDatetime(arg(argv, 0), value.Date); t.Kind() == value.KindTemporal {
			ms, _, _ := t.AsTemporal()
			return value.Temporal(floorDiv(ms, MSPerDay)*MSPerDay, value.Date)
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
		if s, ok := arg(argv, 0).AsStr(); ok {
			if months, days, ms, ok := ParseISODuration(s); ok {
				return value.Duration(months, days, ms)
			}
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
	case FuncLower, FuncUpper:
		s, ok := arg(argv, 0).AsStr()
		if !ok {
			return value.Null()
		}
		if op == FuncLower {
			return value.Str(strings.ToLower(s))
		}
		return value.Str(strings.ToUpper(s))
	case FuncCharLength:
		if s, ok := arg(argv, 0).AsStr(); ok {
			return value.Int(int64(utf8.RuneCountInString(s)))
		}
		return value.Null()
	case FuncCardinality:
		if l, ok := arg(argv, 0).AsList(); ok {
			return value.Int(int64(len(l)))
		}
		if m, ok := arg(argv, 0).AsMap(); ok {
			return value.Int(int64(len(m)))
		}
		return value.Null()
	case FuncNormalize, FuncIsNormalized:
		// NORMALIZE(s [, form]) / s IS [NOT] [form] NORMALIZED (the
		// predicate lowers to is_normalized at parse time). Null
		// propagates; an unknown form name is a null, not an error, per
		// the engine's unknown-input convention.
		s, ok := arg(argv, 0).AsStr()
		if !ok {
			return value.Null()
		}
		form := unorm.NFC
		if len(argv) > 1 {
			fs, ok := arg(argv, 1).AsStr()
			if !ok {
				return value.Null()
			}
			f, ok := unorm.ParseForm(fs)
			if !ok {
				return value.Null()
			}
			form = f
		}
		if op == FuncNormalize {
			return value.Str(unorm.Normalize(s, form))
		}
		return value.Bool(unorm.IsNormalized(s, form))
	case FuncTrim, FuncLTrim, FuncRTrim:
		s, ok := arg(argv, 0).AsStr()
		if !ok {
			return value.Null()
		}
		switch op {
		case FuncTrim:
			return value.Str(strings.TrimSpace(s))
		case FuncLTrim:
			return value.Str(strings.TrimLeft(s, " \t\n\r"))
		default:
			return value.Str(strings.TrimRight(s, " \t\n\r"))
		}
	case FuncMod:
		if a, ok := arg(argv, 0).AsInt(); ok {
			if b, ok := arg(argv, 1).AsInt(); ok && b != 0 {
				return value.Int(a % b)
			}
			if bf, ok := arg(argv, 1).AsFloat(); ok && bf != 0 {
				return value.Float(math.Mod(float64(a), bf))
			}
			return value.Null()
		}
		if a, ok := arg(argv, 0).AsFloat(); ok {
			if b, ok := arg(argv, 1).AsFloat(); ok && b != 0 {
				return value.Float(math.Mod(a, b))
			}
		}
		return value.Null()
	case FuncPower:
		a, ok1 := arg(argv, 0).AsFloat()
		b, ok2 := arg(argv, 1).AsFloat()
		if !ok1 || !ok2 {
			return value.Null()
		}
		return value.Float(math.Pow(a, b))
	case FuncExp, FuncLn, FuncLog10, FuncSin, FuncCos, FuncTan,
		FuncAsin, FuncAcos, FuncAtan, FuncDegrees, FuncRadians:
		x, ok := arg(argv, 0).AsFloat()
		if !ok {
			return value.Null()
		}
		var r float64
		switch op {
		case FuncExp:
			r = math.Exp(x)
		case FuncLn:
			r = math.Log(x)
		case FuncLog10:
			r = math.Log10(x)
		case FuncSin:
			r = math.Sin(x)
		case FuncCos:
			r = math.Cos(x)
		case FuncTan:
			r = math.Tan(x)
		case FuncAsin:
			r = math.Asin(x)
		case FuncAcos:
			r = math.Acos(x)
		case FuncAtan:
			r = math.Atan(x)
		case FuncDegrees:
			r = x * 180 / math.Pi
		default:
			r = x * math.Pi / 180
		}
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return value.Null()
		}
		return value.Float(r)
	case FuncNullIf:
		a, b := arg(argv, 0), arg(argv, 1)
		if value.Equal(a, b) {
			return value.Null()
		}
		return a
	case FuncHead:
		if l, ok := arg(argv, 0).AsList(); ok && len(l) > 0 {
			return l[0]
		}
		return value.Null()
	case FuncLast:
		if l, ok := arg(argv, 0).AsList(); ok && len(l) > 0 {
			return l[len(l)-1]
		}
		return value.Null()
	case FuncTail:
		if l, ok := arg(argv, 0).AsList(); ok {
			if len(l) <= 1 {
				return value.List(nil)
			}
			out := make([]value.Value, len(l)-1)
			copy(out, l[1:])
			return value.List(out)
		}
		return value.Null()
	case FuncElementID:
		if id, ok := arg(argv, 0).AsNode(); ok {
			return value.Str(strconv.FormatUint(uint64(id), 10))
		}
		if pos, ok := arg(argv, 0).AsRel(); ok {
			return value.Str("r" + strconv.FormatUint(uint64(pos), 10))
		}
		return value.Null()
	}
	return value.Null()
}
