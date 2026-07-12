// IN-list hoisting and slot analysis: a batch-constant list bakes to a
// prebuilt membership index (cInConst); a loop-invariant carried list
// rebuilds once per match-call (cInCarried); Slots reports the row slots a
// compiled expression reads for WHERE pushdown.
package compile

import (
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// memKind discriminates an inMembership representation.
type memKind uint8

const (
	// memNodes: an all-node-id list as a roaring bitmap.
	memNodes memKind = iota
	// memHash: hashable scalars keyed by their canonical encoding.
	memHash
	// memLinear: a mixed/unhashable list kept for the exact linear
	// Equal scan (nulls, NaN, non-scalars, temporals -- whose coercions a
	// hash probe cannot reproduce).
	memLinear
)

// inMembership is a prebuilt membership index over an IN list, picking the
// representation by element type; all three are value-identical to the
// plain IN evaluation.
type inMembership struct {
	kind memKind
	// nodes is a sorted deduplicated id slice: IN lists are usually
	// small-to-moderate, so a contiguous binary search beats a
	// compressed-set container walk per probe.
	nodes   []uint32
	hash    map[string]struct{}
	linear  []value.Value
	hasNull bool
	// probe is a reused key buffer for memHash probes: resultFor runs
	// once per row in a single execution's goroutine, so the encoded key
	// need not escape between probes. A map lookup on string(probe) does
	// not allocate.
	probe []byte
}

// memKey projects a value to its membership key, comma-ok false when it
// cannot participate in a hash probe (null, NaN, or a non-scalar --
// temporals coerce against ints under Equal, so they scan linearly).
// An integral float keys as the equal integer, mirroring Equal.
func memKey(b []byte, v value.Value) ([]byte, bool) {
	switch v.Kind() {
	case value.KindBool, value.KindInt, value.KindStr, value.KindNode, value.KindRel:
		return value.AppendKey(b, v), true
	case value.KindFloat:
		f, _ := v.AsFloat()
		if f != f { // NaN
			return b, false
		}
		return value.AppendKey(b, v), true
	default:
		return b, false
	}
}

// buildMembership indexes an evaluated IN list.
func buildMembership(items []value.Value) inMembership {
	allNodes := len(items) > 0
	for _, v := range items {
		if v.Kind() != value.KindNode {
			allNodes = false
			break
		}
	}
	if allNodes {
		ids := make([]uint32, len(items))
		for i, v := range items {
			id, _ := v.AsNode()
			ids[i] = uint32(id)
		}
		slices.Sort(ids)
		return inMembership{kind: memNodes, nodes: slices.Compact(ids)}
	}
	hash := make(map[string]struct{}, len(items))
	for _, v := range items {
		key, ok := memKey(nil, v)
		if !ok {
			// An unhashable element forces the exact linear fallback.
			hasNull := false
			for _, x := range items {
				if x.IsNull() {
					hasNull = true
					break
				}
			}
			return inMembership{kind: memLinear, linear: items, hasNull: hasNull}
		}
		hash[string(key)] = struct{}{}
	}
	return inMembership{kind: memHash, hash: hash}
}

// resultFor is the openCypher IN result for a non-null probe: true on a
// hit, else null when the list contained a null element, else false.
func (m *inMembership) resultFor(v value.Value) value.Value {
	hit := false
	switch m.kind {
	case memNodes:
		if id, ok := v.AsNode(); ok {
			_, hit = slices.BinarySearch(m.nodes, uint32(id))
		}
	case memHash:
		if key, ok := memKey(m.probe[:0], v); ok {
			m.probe = key
			_, hit = m.hash[string(key)]
		}
	default:
		for _, x := range m.linear {
			if value.Equal(x, v) {
				hit = true
				break
			}
		}
	}
	switch {
	case hit:
		return value.Bool(true)
	case m.hasNull:
		return value.Null()
	}
	return value.Bool(false)
}

// Slots collects the row slots a compiled expression reads; hasSlow is set
// for nodes whose inner references aren't slot-resolved (interpreter
// fallbacks, functions, subqueries without a memo set). A memoized
// subquery reads exactly its memo slots, so it can push down to where its
// correlated bindings bind.
func Slots(c *Compiled) (refs []int, hasSlow bool) {
	slotsOf(c.c, &refs, &hasSlow)
	return refs, hasSlow
}

func slotsOf(c cnode, out *[]int, hasSlow *bool) {
	switch n := c.(type) {
	case *cLit:
	case *cSlot:
		*out = append(*out, n.s)
	case *cProp:
		*out = append(*out, n.slot)
	case *cCmpPropConst:
		*out = append(*out, n.prop.slot)
	case *cNot:
		slotsOf(n.e, out, hasSlow)
	case *cNeg:
		slotsOf(n.e, out, hasSlow)
	case *cBin:
		slotsOf(n.l, out, hasSlow)
		slotsOf(n.r, out, hasSlow)
	case *cList:
		for _, x := range n.xs {
			slotsOf(x, out, hasSlow)
		}
	case *cIn:
		slotsOf(n.e, out, hasSlow)
		slotsOf(n.list, out, hasSlow)
	case *cInConst:
		slotsOf(n.e, out, hasSlow)
	case *cInCarried:
		slotsOf(n.e, out, hasSlow)
		slotsOf(n.list, out, hasSlow)
	case *cIsNull:
		slotsOf(n.e, out, hasSlow)
	case *cCase:
		if n.operand != nil {
			slotsOf(n.operand, out, hasSlow)
		}
		for _, w := range n.whens {
			slotsOf(w[0], out, hasSlow)
			slotsOf(w[1], out, hasSlow)
		}
		if n.els != nil {
			slotsOf(n.els, out, hasSlow)
		}
	case *cSubquery:
		if n.hasMemo {
			*out = append(*out, n.memoSlots...)
		} else {
			*hasSlow = true
		}
	default:
		// cFunc keeps conservative last-level placement (its args may read
		// var-length rel slots not tracked for pushdown); cSlow likewise.
		*hasSlow = true
	}
}

// HoistConstIn rewrites IN nodes whose list is invariant over the row
// batch (every slot it reads is batch-constant, no slow node) into a baked
// membership index, evaluated once against a sample row.
func HoistConstIn(ctx *eval.Ctx, c *Compiled, isConst func(int) bool, sample []value.Value, slots map[string]int) *Compiled {
	return newCompiled(hoistConst(ctx, c.c, c.g, isConst, sample, slots), c.g)
}

func hoistConst(ctx *eval.Ctx, c cnode, g *chickpeas.Snapshot, isConst func(int) bool, sample []value.Value, slots map[string]int) cnode {
	switch n := c.(type) {
	case *cNot:
		return &cNot{e: hoistConst(ctx, n.e, g, isConst, sample, slots)}
	case *cNeg:
		return &cNeg{e: hoistConst(ctx, n.e, g, isConst, sample, slots)}
	case *cBin:
		return &cBin{op: n.op, l: hoistConst(ctx, n.l, g, isConst, sample, slots), r: hoistConst(ctx, n.r, g, isConst, sample, slots)}
	case *cList:
		xs := make([]cnode, len(n.xs))
		for i, x := range n.xs {
			xs[i] = hoistConst(ctx, x, g, isConst, sample, slots)
		}
		return &cList{xs: xs}
	case *cIsNull:
		return &cIsNull{e: hoistConst(ctx, n.e, g, isConst, sample, slots), negated: n.negated}
	case *cIn:
		var refs []int
		hasSlow := false
		slotsOf(n.list, &refs, &hasSlow)
		allConst := !hasSlow
		for _, s := range refs {
			if !isConst(s) {
				allConst = false
				break
			}
		}
		if allConst {
			if xs, ok := ceval(ctx, n.list, g, sample, slots).AsList(); ok {
				return &cInConst{
					e: hoistConst(ctx, n.e, g, isConst, sample, slots),
					m: buildMembership(xs),
				}
			}
		}
		return &cIn{
			e:    hoistConst(ctx, n.e, g, isConst, sample, slots),
			list: hoistConst(ctx, n.list, g, isConst, sample, slots),
		}
	default:
		return c
	}
}

// HoistCarriedIn rewrites remaining IN nodes whose list reads only
// carried-in slots (loop-invariant per match-call, not batch-constant)
// into the per-epoch cached form. Applied after HoistConstIn so
// batch-constant lists keep their cheaper baked set.
func HoistCarriedIn(c *Compiled, isCarried func(int) bool) *Compiled {
	return newCompiled(hoistCarried(c.c, isCarried), c.g)
}

func hoistCarried(c cnode, isCarried func(int) bool) cnode {
	switch n := c.(type) {
	case *cNot:
		return &cNot{e: hoistCarried(n.e, isCarried)}
	case *cNeg:
		return &cNeg{e: hoistCarried(n.e, isCarried)}
	case *cBin:
		return &cBin{op: n.op, l: hoistCarried(n.l, isCarried), r: hoistCarried(n.r, isCarried)}
	case *cList:
		xs := make([]cnode, len(n.xs))
		for i, x := range n.xs {
			xs[i] = hoistCarried(x, isCarried)
		}
		return &cList{xs: xs}
	case *cIsNull:
		return &cIsNull{e: hoistCarried(n.e, isCarried), negated: n.negated}
	case *cIn:
		var refs []int
		hasSlow := false
		slotsOf(n.list, &refs, &hasSlow)
		if !hasSlow && len(refs) > 0 {
			carried := true
			for _, s := range refs {
				if !isCarried(s) {
					carried = false
					break
				}
			}
			if carried {
				return &cInCarried{e: hoistCarried(n.e, isCarried), list: n.list}
			}
		}
		return &cIn{e: hoistCarried(n.e, isCarried), list: hoistCarried(n.list, isCarried)}
	default:
		return c
	}
}
