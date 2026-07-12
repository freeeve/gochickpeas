// Non-aggregated projection: the terminal sink evaluates the output
// columns per pushed row, applies DISTINCT on arrival (before ORDER
// BY/LIMIT, as the standard requires -- first occurrence kept), then
// finalize sorts and paginates. Projected rows live in a chunked arena;
// the matched row is retained (arena-copied) only when an ORDER BY key is
// not a projected column. The Rust bounded top-k heap remains a possible
// follow-up with byte-identical results.
package exec

import (
	"maps"
	"math"
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// projSink is the non-aggregated terminal sink.
type projSink struct {
	ctx     *eval.Ctx
	proj    *plan.ProjPlan
	slots   map[string]int
	returns []RowEval
	outs    [][]value.Value
	oArena  rowArena
	// needM: an ORDER BY key evaluates over the matched row, so matched
	// rows must be retained alongside their projections.
	needM  bool
	ms     [][]value.Value
	mArena rowArena
	// DISTINCT state: a single output column dedups through distinctSet
	// (the u32 entity-id fast path for node/rel values, AppendKey bytes
	// otherwise); a multi-column row keys on the concatenated AppendKey
	// encoding -- both dedups thus share one canonical value encoding.
	seenOne *distinctSet
	seen    map[string]struct{}
	key     []byte
	// topk is the streaming bounded accumulator under ORDER BY + LIMIT:
	// at most skip+limit rows are retained, ordered by the sort's exact
	// total order (keys, then arrival), so a rejected row costs one
	// comparison and leaves nothing in the arenas. limitCap is the
	// LIMIT-without-ORDER-BY retention cap (arrival order IS the output
	// order, so rows past skip+limit can never surface).
	topk     *topKRows
	limitCap int
	// topk key evaluation state (mirrors sortRowsByOrder's scope).
	kColIdx []int
	kScope  map[string]int
	kBase   int
	kRowbuf []value.Value
	kBuf    []value.Value
}

func newProjSink(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, width int) *projSink {
	p := &projSink{
		ctx: ctx, proj: proj, slots: slots,
		returns:  make([]RowEval, len(proj.Returns)),
		oArena:   rowArena{width: len(proj.Returns)},
		limitCap: -1,
	}
	for i, r := range proj.Returns {
		p.returns[i] = compileEval(ctx, r.Expr, slots)
	}
	for i := range proj.OrderBy {
		if plan.OrderColIndex(proj.OrderBy[i].Expr, proj.Columns, proj.Returns) < 0 {
			p.needM = true
			p.mArena = rowArena{width: width}
			break
		}
	}
	if proj.Distinct {
		if len(proj.Returns) == 1 {
			p.seenOne = &distinctSet{}
		} else {
			p.seen = map[string]struct{}{}
		}
	}
	if bound := orderBound(proj); bound >= 0 {
		if nk := len(proj.OrderBy); nk > 0 {
			p.topk = newTopKRows(bound, nk, proj.OrderBy)
			p.kColIdx = make([]int, nk)
			for k := range proj.OrderBy {
				p.kColIdx[k] = plan.OrderColIndex(proj.OrderBy[k].Expr, proj.Columns, proj.Returns)
			}
			p.kScope = make(map[string]int, len(slots)+len(proj.Columns))
			maps.Copy(p.kScope, slots)
			p.kBase = 0
			if p.needM {
				p.kBase = width
			}
			for i, c := range proj.Columns {
				p.kScope[c] = p.kBase + i
			}
			p.kBuf = make([]value.Value, nk)
		} else {
			// No ORDER BY: arrival order is output order, so retention
			// past skip+limit can never surface.
			p.limitCap = bound
		}
	}
	return p
}

// pushKeys evaluates the ORDER BY key vector for a just-projected row
// into p.kBuf (out is the projected row, row the matched row).
func (p *projSink) pushKeys(out, row []value.Value) []value.Value {
	built := false
	for k := range p.proj.OrderBy {
		if idx := p.kColIdx[k]; idx >= 0 {
			p.kBuf[k] = out[idx]
			continue
		}
		if !built {
			p.kRowbuf = append(p.kRowbuf[:0], row...)
			p.kRowbuf = append(p.kRowbuf, out...)
			built = true
		}
		p.kBuf[k] = eval.Eval(p.ctx, p.proj.OrderBy[k].Expr, p.kRowbuf, p.kScope)
	}
	return p.kBuf
}

func (p *projSink) push(row []value.Value) {
	if p.limitCap >= 0 && len(p.outs) >= p.limitCap {
		return
	}
	out := p.oArena.alloc()
	for i, c := range p.returns {
		out[i] = c.Eval(p.ctx, row, p.slots)
	}
	if p.seenOne != nil {
		if !p.seenOne.add(out[0], &p.key) {
			p.oArena.rollback()
			return
		}
	} else if p.seen != nil {
		p.key = p.key[:0]
		for _, v := range out {
			p.key = value.AppendKey(p.key, v)
		}
		if _, dup := p.seen[string(p.key)]; dup {
			p.oArena.rollback()
			return
		}
		p.seen[string(p.key)] = struct{}{}
	}
	if p.topk != nil {
		// Keys evaluate here, against the live matched row -- so the
		// matched row never needs retaining at all on this path.
		if !p.topk.offer(p.pushKeys(out, row), out) {
			p.oArena.rollback()
		}
		return
	}
	p.outs = append(p.outs, out)
	if p.needM {
		p.ms = append(p.ms, p.mArena.copyRow(row))
	}
}

func (p *projSink) close() {}

// topKRows is the sink's bounded ORDER BY + LIMIT accumulator: at most
// bound rows survive, under the same total order finalize's sort applies
// (ORDER BY keys, then arrival sequence), so streaming selection equals
// materialize-sort-truncate exactly. A max-heap over parallel arrays;
// the worst survivor sits at the root and each rejected offer costs one
// comparison.
type topKRows struct {
	bound int
	nk    int
	desc  []bool
	keys  []value.Value // n*nk, admitted key vectors
	outs  [][]value.Value
	seqs  []int
	n     int
	seq   int
}

func newTopKRows(bound, nk int, order []ast.SortItem) *topKRows {
	t := &topKRows{bound: bound, nk: nk, desc: make([]bool, nk)}
	for i := range order {
		t.desc[i] = order[i].Desc
	}
	return t
}

// cmpTo compares candidate (keys, seq) against admitted entry i.
func (t *topKRows) cmpTo(keys []value.Value, seq, i int) int {
	for k := 0; k < t.nk; k++ {
		ord := value.OrderCmp(keys[k], t.keys[i*t.nk+k])
		if t.desc[k] {
			ord = -ord
		}
		if ord != 0 {
			return ord
		}
	}
	return seq - t.seqs[i]
}

// cmpEntries compares admitted entries a and b.
func (t *topKRows) cmpEntries(a, b int) int {
	return t.cmpTo(t.keys[a*t.nk:(a+1)*t.nk], t.seqs[a], b)
}

// offer admits the row when it belongs in the current top bound, copying
// its keys (the caller's buffer is reused). Reports whether the row was
// retained.
func (t *topKRows) offer(keys []value.Value, out []value.Value) bool {
	seq := t.seq
	t.seq++
	if t.bound == 0 {
		return false
	}
	if t.n == t.bound && t.cmpTo(keys, seq, 0) >= 0 {
		return false
	}
	if t.n < t.bound {
		t.keys = append(t.keys, keys...)
		t.outs = append(t.outs, out)
		t.seqs = append(t.seqs, seq)
		t.n++
		t.siftUp(t.n - 1)
		return true
	}
	copy(t.keys[:t.nk], keys)
	t.outs[0], t.seqs[0] = out, seq
	t.siftDown(0)
	return true
}

func (t *topKRows) swap(a, b int) {
	for k := 0; k < t.nk; k++ {
		t.keys[a*t.nk+k], t.keys[b*t.nk+k] = t.keys[b*t.nk+k], t.keys[a*t.nk+k]
	}
	t.outs[a], t.outs[b] = t.outs[b], t.outs[a]
	t.seqs[a], t.seqs[b] = t.seqs[b], t.seqs[a]
}

func (t *topKRows) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if t.cmpEntries(i, parent) <= 0 {
			return
		}
		t.swap(i, parent)
		i = parent
	}
}

func (t *topKRows) siftDown(i int) {
	for {
		l := 2*i + 1
		if l >= t.n {
			return
		}
		big := l
		if r := l + 1; r < t.n && t.cmpEntries(r, l) > 0 {
			big = r
		}
		if t.cmpEntries(big, i) <= 0 {
			return
		}
		t.swap(i, big)
		i = big
	}
}

// sorted returns the survivors in final order (ascending under the total
// order).
func (t *topKRows) sorted() [][]value.Value {
	idx := make([]int, t.n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, t.cmpEntries)
	outs := make([][]value.Value, t.n)
	for i, j := range idx {
		outs[i] = t.outs[j]
	}
	return outs
}

func (p *projSink) finalize() [][]value.Value {
	if p.topk != nil {
		return paginate(p.topk.sorted(), p.proj.Skip, p.proj.Limit)
	}
	outs := p.outs
	if len(p.proj.OrderBy) > 0 {
		matchedAt := func(int) []value.Value { return nil }
		base := 0
		if p.needM {
			matchedAt = func(i int) []value.Value { return p.ms[i] }
			if len(p.ms) > 0 {
				base = len(p.ms[0])
			}
		}
		outs = sortRowsByOrder(p.ctx, p.proj, p.slots, matchedAt, base, outs)
	}
	return paginate(outs, p.proj.Skip, p.proj.Limit)
}

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
