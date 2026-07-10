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
	"slices"

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
}

func newProjSink(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, width int) *projSink {
	p := &projSink{
		ctx: ctx, proj: proj, slots: slots,
		returns: make([]RowEval, len(proj.Returns)),
		oArena:  rowArena{width: len(proj.Returns)},
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
	return p
}

func (p *projSink) push(row []value.Value) {
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
	p.outs = append(p.outs, out)
	if p.needM {
		p.ms = append(p.ms, p.mArena.copyRow(row))
	}
}

func (p *projSink) close() {}

func (p *projSink) finalize() [][]value.Value {
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
	// The index tiebreak makes cmp a total order, so an unstable generic
	// sort (no reflection-based swapping) reproduces stable-sort output
	// exactly -- and a total order is what lets the bounded selection
	// below equal sort-then-truncate.
	cmp := func(a, b int) int {
		ka, kb := a*nk, b*nk
		for k := range proj.OrderBy {
			ord := value.OrderCmp(keys[ka+k], keys[kb+k])
			if proj.OrderBy[k].Desc {
				ord = -ord
			}
			if ord != 0 {
				return ord
			}
		}
		return a - b
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
