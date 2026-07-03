// List-scoped expression forms: the all/any/none/single quantifiers,
// reduce folds, and list comprehensions. Each binds its iteration
// variable in a slot appended past the current row.
package eval

import (
	"maps"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// extendRow binds extra variables in slots appended past row, returning
// the extended row and slot map (copies; the originals are untouched).
func extendRow(row []value.Value, slots map[string]int, vars ...string) ([]value.Value, map[string]int, []int) {
	inner := make(map[string]int, len(slots)+len(vars))
	maps.Copy(inner, slots)
	extended := make([]value.Value, len(row), len(row)+len(vars))
	copy(extended, row)
	idx := make([]int, len(vars))
	for i, v := range vars {
		idx[i] = len(extended)
		inner[v] = len(extended)
		extended = append(extended, value.Null())
	}
	return extended, inner, idx
}

// evalListPred evaluates all/any/none/single(var IN list WHERE pred):
// fold the inner predicate under three-valued logic -- a null predicate
// result makes the quantifier null only when the definite matches don't
// decide it. A non-list source is null. Empty-list folds: all/none true,
// any/single false.
func evalListPred(ctx *Ctx, e *ast.ListPred, row []value.Value, slots map[string]int) value.Value {
	items, ok := Eval(ctx, e.List, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := extendRow(row, slots, e.Var)
	nTrue, nNull := 0, 0
	for _, el := range items {
		inner[idx[0]] = el
		truth, known := value.ThreeValued(Eval(ctx, e.Pred, inner, innerSlots))
		switch {
		case !known:
			nNull++
		case truth:
			nTrue++
		}
	}
	nFalse := len(items) - nTrue - nNull
	definite := nNull == 0
	switch e.Quant {
	case ast.QuantAll:
		if nFalse > 0 {
			return value.Bool(false)
		}
		return boolOrNull(definite, true)
	case ast.QuantAny:
		if nTrue > 0 {
			return value.Bool(true)
		}
		return boolOrNull(definite, false)
	case ast.QuantNone:
		if nTrue > 0 {
			return value.Bool(false)
		}
		return boolOrNull(definite, true)
	default: // ast.QuantSingle
		if nTrue > 1 {
			return value.Bool(false)
		}
		return boolOrNull(definite, nTrue == 1)
	}
}

// boolOrNull is Bool(v) when the result is definite, else Null (unknown
// due to a null predicate result that could still change the outcome).
func boolOrNull(definite, v bool) value.Value {
	if definite {
		return value.Bool(v)
	}
	return value.Null()
}

// evalReduce evaluates reduce(acc = init, var IN list | body) as a left
// fold; a non-list source is null, an empty list returns the initial
// accumulator unchanged.
func evalReduce(ctx *Ctx, e *ast.Reduce, row []value.Value, slots map[string]int) value.Value {
	items, ok := Eval(ctx, e.List, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := extendRow(row, slots, e.Acc, e.Var)
	inner[idx[0]] = Eval(ctx, e.Init, row, slots)
	for _, el := range items {
		inner[idx[1]] = el
		inner[idx[0]] = Eval(ctx, e.Body, inner, innerSlots)
	}
	return inner[idx[0]]
}

// evalListComp evaluates [var IN list WHERE filter | map]: keep elements
// passing the filter (when present) and collect the mapped value (or the
// element itself). A non-list source is null.
func evalListComp(ctx *Ctx, e *ast.ListComp, row []value.Value, slots map[string]int) value.Value {
	items, ok := Eval(ctx, e.List, row, slots).AsList()
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := extendRow(row, slots, e.Var)
	var out []value.Value
	for _, el := range items {
		inner[idx[0]] = el
		if e.Filter != nil && !Eval(ctx, e.Filter, inner, innerSlots).IsTruthy() {
			continue
		}
		if e.Map != nil {
			out = append(out, Eval(ctx, e.Map, inner, innerSlots))
		} else {
			out = append(out, el)
		}
	}
	return value.List(out)
}
