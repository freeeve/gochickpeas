// The core interpreter: evaluate a bound expression against a slot-indexed
// row. Aggregate functions are handled by the executor, never here (they
// evaluate to Null if reached).
package eval

import (
	"math"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Eval evaluates expr against row (slot-indexed bindings).
func Eval(ctx *Ctx, expr ast.Expr, row []value.Value, slots map[string]int) value.Value {
	switch e := expr.(type) {
	case *ast.Lit:
		return LitValue(ctx, e.Value)
	case *ast.Var:
		return slotValue(row, slots, e.Name)
	case *ast.Prop:
		return propRead(ctx, slotValue(row, slots, e.Var), e.Key)
	case *ast.Unary:
		v := Eval(ctx, e.Expr, row, slots)
		switch e.Op {
		case ast.Not:
			if b, ok := v.AsBool(); ok {
				return value.Bool(!b)
			}
			return value.Null()
		default: // ast.Neg
			if i, ok := v.AsInt(); ok {
				// -MinInt64 overflows; Null rather than wrap.
				if i == math.MinInt64 {
					return value.Null()
				}
				return value.Int(-i)
			}
			if f, isF := v.AsFloat(); isF && v.Kind() == value.KindFloat {
				return value.Float(-f)
			}
			return value.Null()
		}
	case *ast.Binary:
		return evalBinary(ctx, e, row, slots)
	case *ast.ListExpr:
		out := make([]value.Value, len(e.Elems))
		for i, el := range e.Elems {
			out[i] = Eval(ctx, el, row, slots)
		}
		return value.List(out)
	case *ast.In:
		return evalIn(ctx, e, row, slots)
	case *ast.IsTruth:
		v := Eval(ctx, e.Expr, row, slots)
		b, isBool := v.AsBool()
		got := isBool && b == e.Want
		return value.Bool(got != e.Negated)
	case *ast.IsTyped:
		v := Eval(ctx, e.Expr, row, slots)
		var got bool
		switch e.Kind {
		case "integer":
			got = v.Kind() == value.KindInt
		case "float":
			got = v.Kind() == value.KindFloat
		case "string":
			got = v.Kind() == value.KindStr
		case "boolean":
			got = v.Kind() == value.KindBool
		case "list":
			got = v.Kind() == value.KindList
		case "node":
			got = v.Kind() == value.KindNode
		case "relationship":
			got = v.Kind() == value.KindRel
		}
		return value.Bool(got != e.Negated)
	case *ast.IsNull:
		v := Eval(ctx, e.Expr, row, slots)
		return value.Bool(v.IsNull() != e.Negated)
	case *ast.Case:
		return evalCase(ctx, e, row, slots)
	case *ast.Exists:
		return value.Bool(evalExists(ctx, e.Pattern, e.Where, row, slots))
	case *ast.CountSub:
		return value.Int(evalCountSub(ctx, e.Pattern, e.Where, row, slots))
	case *ast.PatternComp:
		return evalPatternComp(ctx, e, row, slots)
	case *ast.Cost:
		// Weighted shortest-path cost has no GQL surface; the CALL-proc
		// route lands with the traversal milestone. TODO(M17): dispatch
		// through the shortest-path kernels.
		return value.Null()
	case *ast.Func:
		return evalScalarFunc(ctx, e, row, slots)
	case *ast.ListPred:
		return evalListPred(ctx, e, row, slots)
	case *ast.Reduce:
		return evalReduce(ctx, e, row, slots)
	case *ast.ListComp:
		return evalListComp(ctx, e, row, slots)
	case *ast.Index:
		return evalIndex(ctx, e, row, slots)
	case *ast.Slice:
		return evalSlice(ctx, e, row, slots)
	case *ast.PropOf:
		return propRead(ctx, Eval(ctx, e.Base, row, slots), e.Key)
	case *ast.MapProj:
		return evalMapProj(ctx, e, row, slots)
	case *ast.MapLit:
		entries := make([]value.MapEntry, len(e.Fields))
		for i, f := range e.Fields {
			entries[i] = value.MapEntry{Key: f.Key, Val: Eval(ctx, f.Val, row, slots)}
		}
		return value.Map(entries)
	case *ast.HasLabelExpr:
		if n, ok := slotValue(row, slots, e.Var).AsNode(); ok {
			return value.Bool(evalLabelExpr(ctx, n, e.Expr))
		}
		// Unbound (e.g. a null from OPTIONAL MATCH) or non-node: null.
		return value.Null()
	}
	return value.Null()
}

// slotValue reads a variable's bound value from the row; unbound is Null.
func slotValue(row []value.Value, slots map[string]int, name string) value.Value {
	if s, ok := slots[name]; ok && s >= 0 && s < len(row) {
		return row[s]
	}
	return value.Null()
}

// propRead is property access on any base value: a node or rel reads the
// graph; a map reads its entries; a temporal reads a component; an i64
// reads as epoch millis with the accessor as the type signal (someInt.year
// would otherwise be a type error yielding null, so the temporal reading
// is the only useful one). Anything else is Null.
func propRead(ctx *Ctx, base value.Value, key string) value.Value {
	switch base.Kind() {
	case value.KindNode:
		n, _ := base.AsNode()
		if v, ok := ctx.G.NodeProp(n, key); ok {
			return v
		}
	case value.KindRel:
		p, _ := base.AsRel()
		if v, ok := ctx.G.RelProp(p, key); ok {
			return v
		}
	case value.KindMap:
		entries, _ := base.AsMap()
		return mapGet(entries, key)
	case value.KindTemporal:
		ms, _, _ := base.AsTemporal()
		return temporalComponentOf(ms, key)
	case value.KindInt:
		ms, _ := base.AsInt()
		return temporalComponentOf(ms, key)
	case value.KindDuration:
		mo, d, ms, _ := base.AsDuration()
		if c, ok := DurationComponent(mo, d, ms, key); ok {
			return value.Int(c)
		}
	}
	return value.Null()
}

// mapGet looks a key up in a map value's entries (case-sensitive),
// returning the field or Null when absent.
func mapGet(entries []value.MapEntry, key string) value.Value {
	for _, e := range entries {
		if e.Key == key {
			return e.Val
		}
	}
	return value.Null()
}

// temporalComponentOf reads an integer as epoch milliseconds and returns
// the named component; an unknown component is Null.
func temporalComponentOf(millis int64, key string) value.Value {
	if c, ok := Component(millis, key); ok {
		return value.Int(c)
	}
	return value.Null()
}

// evalLabelExpr evaluates a boolean label expression against a node.
func evalLabelExpr(ctx *Ctx, n uint32, e *ast.LabelExpr) bool {
	switch e.Kind {
	case ast.LabelName:
		return ctx.G.HasLabel(n, e.Name)
	case ast.LabelWild:
		// %: the node carries at least one label (label counts are small).
		for _, l := range ctx.G.LabelNames() {
			if ctx.G.HasLabel(n, l) {
				return true
			}
		}
		return false
	case ast.LabelAnd:
		return evalLabelExpr(ctx, n, e.L) && evalLabelExpr(ctx, n, e.R)
	case ast.LabelOr:
		return evalLabelExpr(ctx, n, e.L) || evalLabelExpr(ctx, n, e.R)
	default: // ast.LabelNot
		return !evalLabelExpr(ctx, n, e.L)
	}
}

// evalIn is `expr IN list` with openCypher null semantics: a null operand
// is null; a miss is null (not false) when the list contains a null
// element; a non-list operand is null.
func evalIn(ctx *Ctx, e *ast.In, row []value.Value, slots map[string]int) value.Value {
	v := Eval(ctx, e.Expr, row, slots)
	if v.IsNull() {
		return value.Null()
	}
	xs, ok := Eval(ctx, e.List, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	sawNull := false
	for _, x := range xs {
		if value.Equal(x, v) {
			return value.Bool(true)
		}
		if x.IsNull() {
			sawNull = true
		}
	}
	if sawNull {
		return value.Null()
	}
	return value.Bool(false)
}

// evalCase: the simple form compares the operand for equality against each
// WHEN value; the searched form takes the first truthy WHEN condition.
func evalCase(ctx *Ctx, e *ast.Case, row []value.Value, slots map[string]int) value.Value {
	if e.Operand != nil {
		target := Eval(ctx, e.Operand, row, slots)
		for _, w := range e.Whens {
			if value.Equal(Eval(ctx, w.Cond, row, slots), target) {
				return Eval(ctx, w.Result, row, slots)
			}
		}
	} else {
		for _, w := range e.Whens {
			if Eval(ctx, w.Cond, row, slots).IsTruthy() {
				return Eval(ctx, w.Result, row, slots)
			}
		}
	}
	if e.Else != nil {
		return Eval(ctx, e.Else, row, slots)
	}
	return value.Null()
}

// evalIndex is base[index]: 0-based list indexing, a negative index counts
// from the end; out of range or non-list/non-int yields null.
func evalIndex(ctx *Ctx, e *ast.Index, row []value.Value, slots map[string]int) value.Value {
	xs, ok := Eval(ctx, e.Base, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	i, ok := Eval(ctx, e.Idx, row, slots).AsInt()
	if !ok {
		return value.Null()
	}
	if i < 0 {
		i += int64(len(xs))
	}
	if i < 0 || i >= int64(len(xs)) {
		return value.Null()
	}
	return xs[i]
}

// evalSlice is base[from..to]: the sublist [from, to). Omitted bounds
// default to the list ends; negative bounds count from the end; bounds
// clamp and an empty/inverted range yields []. A non-list base or
// non-integer bound yields null.
func evalSlice(ctx *Ctx, e *ast.Slice, row []value.Value, slots map[string]int) value.Value {
	xs, ok := Eval(ctx, e.Base, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	n := int64(len(xs))
	resolve := func(b ast.Expr, dflt int64) (int64, bool) {
		if b == nil {
			return dflt, true
		}
		i, ok := Eval(ctx, b, row, slots).AsInt()
		if !ok {
			return 0, false
		}
		if i < 0 {
			i += n
		}
		return i, true
	}
	lo, ok1 := resolve(e.From, 0)
	hi, ok2 := resolve(e.To, n)
	if !ok1 || !ok2 {
		return value.Null()
	}
	lo = min(max(lo, 0), n)
	hi = min(max(hi, 0), n)
	if lo >= hi {
		return value.List(nil)
	}
	return value.List(xs[lo:hi])
}

// evalMapProj builds a map by reading the base variable's properties
// (.key), evaluating computed fields (name: expr), and expanding .* to
// every property the node carries, preserving written order.
func evalMapProj(ctx *Ctx, e *ast.MapProj, row []value.Value, slots map[string]int) value.Value {
	base := slotValue(row, slots, e.Var)
	entries := make([]value.MapEntry, 0, len(e.Entries))
	for _, en := range e.Entries {
		switch en.Kind {
		case ast.MapProjProp:
			entries = append(entries, value.MapEntry{Key: en.Key, Val: propRead(ctx, base, en.Key)})
		case ast.MapProjField:
			entries = append(entries, value.MapEntry{Key: en.Key, Val: Eval(ctx, en.Expr, row, slots)})
		default: // ast.MapProjAll -- only nodes have enumerable properties.
			if n, ok := base.AsNode(); ok {
				for _, key := range ctx.G.NodePropKeys(n) {
					v, _ := ctx.G.NodeProp(n, key)
					entries = append(entries, value.MapEntry{Key: key, Val: v})
				}
			}
		}
	}
	return value.Map(entries)
}

// evalBinary: AND/OR short-circuit under three-valued logic; comparisons
// go through value.Compare; arithmetic and string predicates below.
func evalBinary(ctx *Ctx, e *ast.Binary, row []value.Value, slots map[string]int) value.Value {
	switch e.Op {
	case ast.OpAnd:
		l, lk := value.ThreeValued(Eval(ctx, e.LHS, row, slots))
		if lk && !l {
			return value.Bool(false)
		}
		r, rk := value.ThreeValued(Eval(ctx, e.RHS, row, slots))
		return value.KleeneAnd(l, lk, r, rk)
	case ast.OpOr:
		l, lk := value.ThreeValued(Eval(ctx, e.LHS, row, slots))
		if lk && l {
			return value.Bool(true)
		}
		r, rk := value.ThreeValued(Eval(ctx, e.RHS, row, slots))
		return value.KleeneOr(l, lk, r, rk)
	case ast.OpXor:
		// Three-valued XOR: null when either side is null (no
		// short-circuit -- unlike AND/OR, one known side never decides).
		l, lk := value.ThreeValued(Eval(ctx, e.LHS, row, slots))
		r, rk := value.ThreeValued(Eval(ctx, e.RHS, row, slots))
		if !lk || !rk {
			return value.Null()
		}
		return value.Bool(l != r)
	}
	l := Eval(ctx, e.LHS, row, slots)
	r := Eval(ctx, e.RHS, row, slots)
	switch e.Op {
	case ast.OpEq:
		return CmpBool(l, r, func(o int) bool { return o == 0 })
	case ast.OpNeq:
		return CmpBool(l, r, func(o int) bool { return o != 0 })
	case ast.OpLt:
		return CmpBool(l, r, func(o int) bool { return o < 0 })
	case ast.OpLte:
		return CmpBool(l, r, func(o int) bool { return o <= 0 })
	case ast.OpGt:
		return CmpBool(l, r, func(o int) bool { return o > 0 })
	case ast.OpGte:
		return CmpBool(l, r, func(o int) bool { return o >= 0 })
	case ast.OpConcat:
		return Concat(l, r)
	case ast.OpStartsWith, ast.OpEndsWith, ast.OpContains:
		return StrPred(e.Op, l, r)
	default:
		return Arith(e.Op, l, r)
	}
}

// CmpBool maps a three-valued comparison through a predicate on the
// ordering; incomparable is Null. Shared with the compiled path.
func CmpBool(l, r value.Value, f func(int) bool) value.Value {
	if o, ok := value.Compare(l, r); ok {
		return value.Bool(f(o))
	}
	return value.Null()
}

// Concat is the || operator: string or list concatenation only -- unlike
// +, it never adds numbers; any other operand pair is Null.
func Concat(l, r value.Value) value.Value {
	if ls, ok := l.AsStr(); ok {
		if rs, ok := r.AsStr(); ok {
			return value.Str(ls + rs)
		}
		return value.Null()
	}
	if ll, ok := l.AsList(); ok {
		if rl, ok := r.AsList(); ok {
			out := make([]value.Value, 0, len(ll)+len(rl))
			out = append(out, ll...)
			out = append(out, rl...)
			return value.List(out)
		}
	}
	return value.Null()
}
