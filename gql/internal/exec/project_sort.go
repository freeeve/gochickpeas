// ORDER BY and pagination for materialized projection rows: a
// decorate-sort-undecorate that evaluates each row's key vector once,
// with a typed fast path (numeric/entity key columns pack into
// order-preserving words so the comparator is pure word compares) and a
// bounded top-k selection under LIMIT. Split from project.go, which holds
// the streaming projection sink.
package exec

import (
	"maps"
	"math"
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// sortRowsByOrder orders outs by proj.OrderBy, decorate-sort-undecorate: each
// row's key vector is evaluated once up front, so the comparator does no
// per-comparison evaluation or allocation (an ORDER BY key would otherwise be
// re-evaluated O(rows log rows) times). matchedAt supplies each row's matched
// prefix (nil when no key needs it) and base is that prefix's width, so a
// non-projected key expression reads a reused combined-row buffer under an
// invariant column scope.
func sortRowsByOrder(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, matchedAt func(int) []value.Value, base int, outs [][]value.Value) [][]value.Value {
	nk := len(proj.OrderBy)
	colIdx := make([]int, nk)
	for k := range proj.OrderBy {
		colIdx[k] = plan.OrderColIndex(proj.OrderBy[k].Expr, proj.Columns, proj.Returns)
	}
	scope := make(map[string]int, len(slots)+len(proj.Columns))
	maps.Copy(scope, slots)
	for i, c := range proj.Columns {
		scope[c] = base + i
	}
	keys := make([]value.Value, len(outs)*nk)
	var rowbuf []value.Value
	for i := range outs {
		built := false
		for k := range proj.OrderBy {
			if colIdx[k] >= 0 {
				keys[i*nk+k] = outs[i][colIdx[k]]
				continue
			}
			if !built {
				rowbuf = append(rowbuf[:0], matchedAt(i)...)
				rowbuf = append(rowbuf, outs[i]...)
				built = true
			}
			keys[i*nk+k] = eval.Eval(ctx, proj.OrderBy[k].Expr, rowbuf, scope)
		}
	}
	idx := make([]int, len(outs))
	for i := range idx {
		idx[i] = i
	}
	// Typed key columns: a key column homogeneous over OrderCmp's numeric
	// tier (Int/Float) or over one entity kind compares through its
	// primitive encoding -- the identical kernel (value.TotalOrderF64 /
	// id order), skipping the boxed rank dispatch per comparison. Any
	// other mix keeps value.OrderCmp.
	const (
		colGeneric = uint8(iota)
		colNumeric
		colEntity
	)
	colClass := make([]uint8, nk)
	var fkeys []float64
	var ukeys []uint64
	if len(outs) > 0 {
		for k := range nk {
			numeric, node, rel := true, true, true
			for i := range outs {
				switch keys[i*nk+k].Kind() {
				case value.KindInt, value.KindFloat:
					node, rel = false, false
				case value.KindNode:
					numeric, rel = false, false
				case value.KindRel:
					numeric, node = false, false
				default:
					numeric, node, rel = false, false, false
				}
				if !numeric && !node && !rel {
					break
				}
			}
			switch {
			case numeric:
				colClass[k] = colNumeric
				if fkeys == nil {
					fkeys = make([]float64, len(outs)*nk)
				}
				for i := range outs {
					fkeys[i*nk+k] = value.OrderNumF64(keys[i*nk+k])
				}
			case node || rel:
				colClass[k] = colEntity
				if ukeys == nil {
					ukeys = make([]uint64, len(outs)*nk)
				}
				for i := range outs {
					var id uint64
					if n, ok := keys[i*nk+k].AsNode(); ok {
						id = uint64(uint32(n))
					} else if p, ok := keys[i*nk+k].AsRel(); ok {
						id = uint64(uint32(p))
					}
					ukeys[i*nk+k] = id
				}
			}
		}
	}
	// The index tiebreak makes cmp a total order, so an unstable generic
	// sort (no reflection-based swapping) reproduces stable-sort output
	// exactly -- and a total order is what lets the bounded selection
	// below equal sort-then-truncate.
	cmp := func(a, b int) int {
		ka, kb := a*nk, b*nk
		for k := range proj.OrderBy {
			var ord int
			switch colClass[k] {
			case colNumeric:
				ord = value.TotalOrderF64(fkeys[ka+k], fkeys[kb+k])
			case colEntity:
				x, y := ukeys[ka+k], ukeys[kb+k]
				switch {
				case x < y:
					ord = -1
				case x > y:
					ord = 1
				}
			default:
				ord = value.OrderCmp(keys[ka+k], keys[kb+k])
			}
			if proj.OrderBy[k].Desc {
				ord = -ord
			}
			if ord != 0 {
				return ord
			}
		}
		return a - b
	}
	// When every key column is typed, the key vector packs into
	// order-preserving uint64 words -- numeric through the IEEE-754
	// totalOrder monotone encoding (the same relation TotalOrderF64
	// computes per comparison, applied once per key instead), entity ids
	// directly, descending baked in by complement -- so the comparator is
	// pure word compares. Result order is identical to the typed
	// comparator above by monotonicity of the encodings.
	allTyped := len(outs) > 0
	for k := range nk {
		if colClass[k] == colGeneric {
			allTyped = false
			break
		}
	}
	if allTyped {
		words := make([]uint64, len(outs)*nk)
		for k := range nk {
			desc := proj.OrderBy[k].Desc
			for i := range outs {
				var w uint64
				if colClass[k] == colNumeric {
					w = packSortWordF64(fkeys[i*nk+k])
				} else {
					w = ukeys[i*nk+k]
				}
				if desc {
					w = ^w
				}
				words[i*nk+k] = w
			}
		}
		cmp = func(a, b int) int {
			ka, kb := a*nk, b*nk
			for k := 0; k < nk; k++ {
				x, y := words[ka+k], words[kb+k]
				if x != y {
					if x < y {
						return -1
					}
					return 1
				}
			}
			return a - b
		}
	}
	// Under ORDER BY + LIMIT, pagination consumes only the leading
	// skip+limit rows: select those with a bounded heap (one comparison
	// per rejected row) instead of sorting everything.
	if bound := orderBound(proj); bound >= 0 && bound < len(idx) {
		idx = topKIdx(idx, bound, cmp)
	}
	slices.SortFunc(idx, cmp)
	sorted := make([][]value.Value, len(idx))
	for i, j := range idx {
		sorted[i] = outs[j]
	}
	return sorted
}

// packSortWordF64 is the IEEE-754 totalOrder monotone uint64 encoding:
// unsigned comparison of the encoded words equals value.TotalOrderF64 on
// the raw floats (negatives complement fully; non-negatives set the sign
// bit), so a packed key column sorts through plain word compares.
func packSortWordF64(f float64) uint64 {
	w := math.Float64bits(f)
	if w&(1<<63) != 0 {
		return ^w
	}
	return w | 1<<63
}

// orderBound is the row count pagination can consume after an ordered
// projection (skip+limit), or -1 when unbounded.
func orderBound(proj *plan.ProjPlan) int {
	if proj.Limit == nil {
		return -1
	}
	k := *proj.Limit
	if proj.Skip != nil {
		k += *proj.Skip
	}
	if k > uint64(^uint(0)>>1) {
		return -1
	}
	return int(k)
}

// topKIdx selects the k smallest elements of idx under the total order
// cmp, order among them unspecified (the caller sorts the survivors). A
// size-k max-heap over the prefix; every further candidate is rejected
// with a single comparison against the current k-th unless it improves
// the set. Equal to sort-then-truncate because cmp is total.
func topKIdx(idx []int, k int, cmp func(a, b int) int) []int {
	if k == 0 {
		return idx[:0]
	}
	h := idx[:k]
	for i := k/2 - 1; i >= 0; i-- {
		siftDownIdx(h, i, cmp)
	}
	for _, cand := range idx[k:] {
		if cmp(cand, h[0]) < 0 {
			h[0] = cand
			siftDownIdx(h, 0, cmp)
		}
	}
	return h
}

// siftDownIdx restores the max-heap property below i.
func siftDownIdx(h []int, i int, cmp func(a, b int) int) {
	for {
		l := 2*i + 1
		if l >= len(h) {
			return
		}
		big := l
		if r := l + 1; r < len(h) && cmp(h[r], h[l]) > 0 {
			big = r
		}
		if cmp(h[big], h[i]) <= 0 {
			return
		}
		h[i], h[big] = h[big], h[i]
		i = big
	}
}

// paginate applies OFFSET/SKIP then LIMIT.
func paginate(v [][]value.Value, skip, limit *uint64) [][]value.Value {
	if skip != nil {
		s := int(*skip)
		if s >= len(v) {
			return nil
		}
		v = v[s:]
	}
	if limit != nil && uint64(len(v)) > *limit {
		v = v[:*limit]
	}
	return v
}
