// Comparison semantics ported from the Rust cypher crate's value.rs: the
// three-valued Compare/Equal used by =, <, >, IN and friends; the Kleene
// combinators behind AND/OR; and the genuinely total OrderCmp for ORDER BY.
package value

import (
	"math"
	"sort"
)

// Compare is the three-valued comparison behind =, <>, <, <=, >, >=: ok is
// false when either side is Null or the types are not comparable. Numbers
// compare across Int/Float by coercing through float64 (including Int/Int,
// mirroring the Rust engine exactly -- lossy above 2^53 only).
func Compare(a, b Value) (int, bool) {
	if a.kind == KindNull || b.kind == KindNull {
		return 0, false
	}
	switch {
	case a.kind == KindBool && b.kind == KindBool:
		return cmpUint(a.num, b.num), true
	case a.kind == KindStr && b.kind == KindStr:
		return cmpStr(a.str, b.str), true
	case a.kind == KindNode && b.kind == KindNode,
		a.kind == KindRel && b.kind == KindRel:
		return cmpUint(a.num, b.num), true
	case a.kind == KindList && b.kind == KindList:
		return compareLists(a.ext.list, b.ext.list)
	case a.kind == KindMap && b.kind == KindMap:
		// Maps are equatable (content, order-independent) but not ordered:
		// equal -> 0, otherwise incomparable, so =/<>/DISTINCT work and
		// </> on maps yield null.
		if mapsEqual(a.ext.entries, b.ext.entries) {
			return 0, true
		}
		return 0, false
	case a.kind == KindPath && b.kind == KindPath:
		// Paths are equatable (same node and rel sequences) but not ordered.
		if slicesEqual(a.ext.nodes, b.ext.nodes) && slicesEqual(a.ext.rels, b.ext.rels) {
			return 0, true
		}
		return 0, false
	// Temporals order by epoch millis and coerce against an i64 column (the
	// stored epoch-millis value) so creationDate < datetime(...) works.
	case a.kind == KindTemporal && b.kind == KindTemporal,
		a.kind == KindTemporal && b.kind == KindInt,
		a.kind == KindInt && b.kind == KindTemporal:
		return cmpInt(int64(a.num), int64(b.num)), true
	case a.kind == KindDuration && b.kind == KindDuration:
		// Durations are equatable by component but not ordered (months vs
		// days is ambiguous).
		if a.ext.months == b.ext.months && a.ext.days == b.ext.days && a.num == b.num {
			return 0, true
		}
		return 0, false
	}
	x, xok := a.asNum()
	y, yok := b.asNum()
	if !xok || !yok {
		return 0, false
	}
	// float64 partial comparison: NaN on either side is incomparable.
	switch {
	case x < y:
		return -1, true
	case x > y:
		return 1, true
	case x == y:
		return 0, true
	}
	return 0, false
}

// Equal reports three-valued equality collapsed to a boolean: true only
// when Compare says equal.
func Equal(a, b Value) bool {
	c, ok := Compare(a, b)
	return ok && c == 0
}

// compareLists compares element-wise via Compare, then by length; an
// incomparable element pair makes the lists incomparable.
func compareLists(x, y []Value) (int, bool) {
	for i := range min(len(x), len(y)) {
		c, ok := Compare(x[i], y[i])
		if !ok || c != 0 {
			return c, ok
		}
	}
	return cmpInt(int64(len(x)), int64(len(y))), true
}

// mapsEqual reports whether two maps have the same key set with equal
// values -- independent of insertion order (GQL map equality).
func mapsEqual(x, y []MapEntry) bool {
	if len(x) != len(y) {
		return false
	}
	for _, e := range x {
		found := false
		for _, f := range y {
			if f.Key == e.Key {
				found = Equal(e.Val, f.Val)
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ThreeValued maps a value to a Kleene truth value for AND/OR: a Bool is
// its value, Null is unknown (known=false). A non-boolean operand maps to
// false, preserving the engine's leniency (only Bool(true) was ever true).
func ThreeValued(v Value) (truth, known bool) {
	switch v.kind {
	case KindBool:
		return v.num != 0, true
	case KindNull:
		return false, false
	default:
		return false, true
	}
}

// KleeneAnd is three-valued AND over ThreeValued operands: false dominates,
// else Null if either side is unknown, else true.
func KleeneAnd(l, lknown, r, rknown bool) Value {
	switch {
	case lknown && !l, rknown && !r:
		return Bool(false)
	case lknown && rknown:
		return Bool(true)
	}
	return Null()
}

// KleeneOr is three-valued OR over ThreeValued operands: true dominates,
// else Null if either side is unknown, else false.
func KleeneOr(l, lknown, r, rknown bool) Value {
	switch {
	case lknown && l, rknown && r:
		return Bool(true)
	case lknown && rknown:
		return Bool(false)
	}
	return Null()
}

// OrderCmp is a genuine total order for ORDER BY: values of different type
// categories order by a stable type rank (Null last), and same-category
// values order naturally. Deliberately NOT Compare (a partial three-valued
// relation): NaN is placed after finite numbers via a total float order,
// and Temporal is never cross-compared with numbers (which produced an
// intransitive cycle in the partial relation); each category is a
// self-contained tier.
func OrderCmp(a, b Value) int {
	ra, rb := orderRank(a), orderRank(b)
	if ra != rb {
		return cmpInt(int64(ra), int64(rb))
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindBool:
		return cmpUint(a.num, b.num)
	case KindInt, KindFloat:
		// Int and Float share a tier; compare numerically with a total
		// order over float64 (NaN sorts after finite values). int64 ->
		// float64 is lossy for very large magnitudes, but only as an ORDER
		// BY tiebreak (equality uses Compare).
		return totalCmpF64(orderNum(a), orderNum(b))
	case KindStr:
		return cmpStr(a.str, b.str)
	case KindList:
		x, y := a.ext.list, b.ext.list
		for i := range min(len(x), len(y)) {
			if c := OrderCmp(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(int64(len(x)), int64(len(y)))
	case KindNode, KindRel:
		return cmpUint(a.num, b.num)
	case KindPath:
		if c := cmpSlices(a.ext.nodes, b.ext.nodes); c != 0 {
			return c
		}
		return cmpSlices(a.ext.rels, b.ext.rels)
	case KindMap:
		return mapOrder(a.ext.entries, b.ext.entries)
	case KindTemporal:
		if c := cmpInt(int64(a.num), int64(b.num)); c != 0 {
			return c
		}
		return cmpInt(int64(a.aux), int64(b.aux))
	case KindDuration:
		if c := cmpInt(a.ext.months, b.ext.months); c != 0 {
			return c
		}
		if c := cmpInt(a.ext.days, b.ext.days); c != 0 {
			return c
		}
		return cmpInt(int64(a.num), int64(b.num))
	}
	return 0
}

// orderRank is OrderCmp's type-tier table (Null last).
func orderRank(v Value) uint8 {
	switch v.kind {
	case KindBool:
		return 0
	case KindInt, KindFloat:
		return 1
	case KindStr:
		return 2
	case KindList:
		return 3
	case KindNode:
		return 4
	case KindRel:
		return 5
	case KindPath:
		return 6
	case KindMap:
		return 7
	case KindTemporal:
		return 8
	case KindDuration:
		return 9
	}
	return math.MaxUint8
}

// orderNum reads the numeric tier's payload, NaN if somehow non-numeric.
func orderNum(v Value) float64 {
	f, ok := v.asNum()
	if !ok {
		return math.NaN()
	}
	return f
}

// totalCmpF64 is IEEE-754 totalOrder (the Rust f64::total_cmp): -NaN <
// -Inf < ... < -0.0 < +0.0 < ... < +Inf < +NaN.
func totalCmpF64(x, y float64) int {
	xb := int64(math.Float64bits(x))
	yb := int64(math.Float64bits(y))
	xb ^= (xb >> 63) & math.MaxInt64
	yb ^= (yb >> 63) & math.MaxInt64
	return cmpInt(xb, yb)
}

// mapOrder is a total order over maps for OrderCmp: compare entries in
// key-sorted order by (key, value), then by size, so two maps order
// deterministically regardless of insertion order (consistent with the
// order-independent map equality in Compare).
func mapOrder(x, y []MapEntry) int {
	xi, yi := keySorted(x), keySorted(y)
	for k := range min(len(xi), len(yi)) {
		ex, ey := x[xi[k]], y[yi[k]]
		if c := cmpStr(ex.Key, ey.Key); c != 0 {
			return c
		}
		if c := OrderCmp(ex.Val, ey.Val); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(x)), int64(len(y)))
}

// keySorted returns the entry indices of m in ascending key order.
func keySorted(m []MapEntry) []int {
	idx := make([]int, len(m))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return m[idx[a]].Key < m[idx[b]].Key })
	return idx
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpSlices compares uint32 slices lexicographically then by length.
func cmpSlices[T ~uint32](x, y []T) int {
	for i := range min(len(x), len(y)) {
		if c := cmpUint(uint64(x[i]), uint64(y[i])); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(x)), int64(len(y)))
}

// slicesEqual reports element-wise equality of uint32 slices.
func slicesEqual[T ~uint32](x, y []T) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
