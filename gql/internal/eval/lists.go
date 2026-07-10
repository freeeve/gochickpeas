// List-scoped expression forms: the all/any/none/single quantifiers,
// reduce folds, and list comprehensions. Each binds its iteration
// variable in a slot appended past the current row.
package eval

import (
	"maps"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// scopeFor binds extra variables in slots appended past row, returning the
// extended row, slot map, and iteration-variable indices. It caches the
// lexically-invariant slot map and idx per node on the Ctx and reuses a
// refilled row buffer, so a list scope evaluated once per outer row (a
// comprehension or quantifier in a hot filter) allocates nothing after the
// first call. The originals are untouched.
func (c *Ctx) scopeFor(node ast.Expr, row []value.Value, slots map[string]int, vars ...string) ([]value.Value, map[string]int, []int) {
	s := c.scopes[node]
	if s == nil || s.baseLen != len(row) {
		s = &scopeScratch{
			slots:   make(map[string]int, len(slots)+len(vars)),
			idx:     make([]int, len(vars)),
			baseLen: len(row),
		}
		maps.Copy(s.slots, slots)
		for i, v := range vars {
			s.idx[i] = len(row) + i
			s.slots[v] = len(row) + i
		}
		if c.scopes == nil {
			c.scopes = map[ast.Expr]*scopeScratch{}
		}
		c.scopes[node] = s
	}
	need := len(row) + len(vars)
	if cap(s.row) < need {
		s.row = make([]value.Value, need)
	}
	s.row = s.row[:need]
	copy(s.row, row)
	for i := range vars {
		s.row[len(row)+i] = value.Null()
	}
	return s.row, s.slots, s.idx
}

// listSeq is a list-source view for the list-scope forms: either a
// materialized list or a lazily iterated integer range, so a range(a,b[,s])
// source -- the common quantifier counter idiom -- never materializes a
// boxed list per evaluation.
type listSeq struct {
	items       []value.Value
	start, step int64
	n           int
	ranged      bool
}

// at returns element i without bounds concern beyond [0, n).
func (s *listSeq) at(i int) value.Value {
	if s.ranged {
		return value.Int(s.start + int64(i)*s.step)
	}
	return s.items[i]
}

// listSource evaluates a list-scope source expression. A range() call
// becomes a lazy sequence under exactly applyRange's semantics (non-int
// bound or zero step is null, a non-int step falls back to 1); anything
// else evaluates eagerly and unwraps as a list. ok=false is a null or
// non-list source.
func listSource(ctx *Ctx, src ast.Expr, row []value.Value, slots map[string]int) (listSeq, bool) {
	if f, isFunc := src.(*ast.Func); isFunc && !f.Star && len(f.Args) >= 2 && len(f.Args) <= 3 {
		if op, known := ResolveFuncOp(f.Name); known && op == FuncRange {
			start, ok1 := Eval(ctx, f.Args[0], row, slots).AsInt()
			end, ok2 := Eval(ctx, f.Args[1], row, slots).AsInt()
			if !ok1 || !ok2 {
				return listSeq{}, false
			}
			step := int64(1)
			if len(f.Args) == 3 {
				if s, ok := Eval(ctx, f.Args[2], row, slots).AsInt(); ok {
					step = s
				}
			}
			if step == 0 {
				return listSeq{}, false
			}
			n := 0
			if (step > 0 && start <= end) || (step < 0 && start >= end) {
				n = int((end-start)/step) + 1
			}
			return listSeq{start: start, step: step, n: n, ranged: true}, true
		}
	}
	items, ok := Eval(ctx, src, row, slots).AsList()
	return listSeq{items: items, n: len(items)}, ok
}

// evalListPred evaluates all/any/none/single(var IN list WHERE pred):
// fold the inner predicate under three-valued logic -- a null predicate
// result makes the quantifier null only when the definite matches don't
// decide it. A non-list source is null. Empty-list folds: all/none true,
// any/single false.
func evalListPred(ctx *Ctx, e *ast.ListPred, row []value.Value, slots map[string]int) value.Value {
	src, ok := listSource(ctx, e.List, row, slots)
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := ctx.scopeFor(e, row, slots, e.Var)
	nTrue, nNull := 0, 0
	for i := 0; i < src.n; i++ {
		inner[idx[0]] = src.at(i)
		truth, known := value.ThreeValued(Eval(ctx, e.Pred, inner, innerSlots))
		switch {
		case !known:
			nNull++
		case truth:
			nTrue++
		}
	}
	nFalse := src.n - nTrue - nNull
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
	src, ok := listSource(ctx, e.List, row, slots)
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := ctx.scopeFor(e, row, slots, e.Acc, e.Var)
	inner[idx[0]] = Eval(ctx, e.Init, row, slots)
	for i := 0; i < src.n; i++ {
		inner[idx[1]] = src.at(i)
		inner[idx[0]] = Eval(ctx, e.Body, inner, innerSlots)
	}
	return inner[idx[0]]
}

// evalListComp evaluates [var IN list WHERE filter | map]: keep elements
// passing the filter (when present) and collect the mapped value (or the
// element itself). A non-list source is null.
func evalListComp(ctx *Ctx, e *ast.ListComp, row []value.Value, slots map[string]int) value.Value {
	src, ok := listSource(ctx, e.List, row, slots)
	if !ok {
		return value.Null()
	}
	inner, innerSlots, idx := ctx.scopeFor(e, row, slots, e.Var)
	var out []value.Value
	for i := 0; i < src.n; i++ {
		el := src.at(i)
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
