// Cross-boundary fusion for [aggregating boundary] -> [identity ORDER BY
// boundary] -> [DISTINCT column-subset + LIMIT]: DISTINCT keeps each
// distinct tuple's FIRST row under the sort's total order and LIMIT keeps
// the leading bound, so each tuple's effective position is the MINIMUM of
// its rows' key vectors (argmin) and the result is a bounded selection
// over those minima. Streaming the aggregate's finalized groups through a
// per-tuple argmin map replaces the full group-row materialization, the
// full-width sort decoration, and the sort itself.
package exec

import (
	"maps"
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// orderedDistinctFusions counts fused chain executions (the engagement
// oracle); disableOrderedDistinct pins differential tests to the general
// materialize-sort-dedup pipeline.
var (
	orderedDistinctFusions int
	disableOrderedDistinct bool
)

// orderedDistinctShape validates the three-segment shape and resolves the
// DISTINCT boundary's column reads. The ordering boundary must be a pure
// identity carrying only ORDER BY (no pagination, no filter), and the
// DISTINCT boundary must be a bare column-subset projection with a LIMIT
// and nothing else -- anything richer falls back to the general path.
func orderedDistinctShape(segs []*plan.Segment, at int) (dcols []int, bound int, ok bool) {
	if at+2 >= len(segs) {
		return nil, 0, false
	}
	agg, ord, dst := segs[at], segs[at+1], segs[at+2]
	if !agg.Proj.Aggregated || agg.Proj.Distinct || len(agg.Proj.OrderBy) != 0 ||
		agg.Proj.Skip != nil || agg.Proj.Limit != nil || agg.PostWhere != nil {
		return nil, 0, false
	}
	if !identityBoundary(ord) || len(ord.Proj.OrderBy) == 0 || ord.Proj.Skip != nil ||
		ord.Proj.Limit != nil || ord.PostWhere != nil || ord.RowWidth != len(agg.Proj.Returns) {
		return nil, 0, false
	}
	if len(dst.Stages) != 0 || !dst.Proj.Distinct || dst.Proj.Aggregated ||
		len(dst.Proj.OrderBy) != 0 || dst.Proj.Limit == nil || dst.PostWhere != nil ||
		len(dst.Proj.Post) != 0 || dst.Proj.NHidden != 0 {
		return nil, 0, false
	}
	dcols = make([]int, len(dst.Proj.Returns))
	for i, r := range dst.Proj.Returns {
		v, isVar := r.Expr.(*ast.Var)
		if !isVar {
			return nil, 0, false
		}
		s, bound := dst.Slots[v.Name]
		if !bound || s < 0 || s >= ord.RowWidth {
			return nil, 0, false
		}
		dcols[i] = s
	}
	b := orderBound(&dst.Proj)
	if b < 0 {
		return nil, 0, false
	}
	return dcols, b, true
}

// tryOrderedDistinctTopK recognizes the shape at segs[at], executes the
// aggregate segment normally, streams its finalized groups through a
// distinct-tuple argmin, bounded-selects the winning tuples, and returns
// the DISTINCT boundary's output. consumed is 3 on success.
func tryOrderedDistinctTopK(ctx *eval.Ctx, segs []*plan.Segment, at int, inputs [][]value.Value) ([][]value.Value, int, bool) {
	if disableOrderedDistinct {
		return nil, 0, false
	}
	dcols, bound, ok := orderedDistinctShape(segs, at)
	if !ok {
		return nil, 0, false
	}
	agg, ord, dst := segs[at], segs[at+1], segs[at+2]
	orderedDistinctFusions++

	// Run the aggregate segment's stage chain into its sink, exactly as
	// runSegmentRun would for a run head.
	term := newAggSink(ctx, &agg.Proj, agg.Slots)
	boundSlots := segmentBoundSlots(agg)
	var sample []value.Value
	if len(inputs) > 0 {
		sample = make([]value.Value, agg.RowWidth)
		copy(sample, inputs[0])
	}
	constIn := func(s int) bool {
		if s < 0 || s >= len(boundSlots) || boundSlots[s] {
			return false
		}
		return slotAgrees(s, inputs, true)
	}
	head := buildChain(ctx, agg, term, constIn, sample)
	buf := make([]value.Value, agg.RowWidth)
	for _, in := range inputs {
		clear(buf)
		copy(buf, in)
		if !head.push(buf) {
			break
		}
	}
	head.close()

	// Order-key evaluation state, mirroring sortRowsByOrder's scope for
	// the identity boundary (columns at base 0; non-column keys evaluate
	// over the finalized group row).
	oproj := &ord.Proj
	nk := len(oproj.OrderBy)
	colIdx := make([]int, nk)
	for k := range oproj.OrderBy {
		colIdx[k] = plan.OrderColIndex(oproj.OrderBy[k].Expr, oproj.Columns, oproj.Returns)
	}
	scope := make(map[string]int, len(ord.Slots)+len(oproj.Columns))
	maps.Copy(scope, ord.Slots)
	for i, c := range oproj.Columns {
		scope[c] = i
	}
	desc := make([]bool, nk)
	for k := range oproj.OrderBy {
		desc[k] = oproj.OrderBy[k].Desc
	}

	// Bounded argmin: a tuple only matters if its minimum key vector can
	// enter the top bound, per-tuple minima only improve, and the bound-th
	// candidate threshold only tightens -- so a row whose key cannot beat
	// the threshold for an UNTRACKED tuple can never matter (any later,
	// better row of that tuple is judged on its own key), and an evicted
	// candidate's stale minimum is irrelevant (it was already worse than
	// the threshold a re-entry would have to beat). State is O(bound):
	// fixed slot arrays, a worst-at-root heap over slots, and a membership
	// map holding only current candidates.
	am := newArgminTopK(bound, nk, len(dcols), desc)
	kbuf := make([]value.Value, nk)
	seq := 0
	term.agg.forEachGroup(ctx, &agg.Proj, func(row []value.Value) {
		for k := range oproj.OrderBy {
			if ci := colIdx[k]; ci >= 0 {
				kbuf[k] = row[ci]
			} else {
				kbuf[k] = eval.Eval(ctx, oproj.OrderBy[k].Expr, row, scope)
			}
		}
		am.offer(row, dcols, kbuf, seq)
		seq++
	})
	return paginate(am.sorted(), dst.Proj.Skip, dst.Proj.Limit), 3, true
}

// argminTopK is the bounded per-tuple argmin accumulator: at most bound
// distinct tuples are tracked, each with its best (minimum) key vector
// and that row's sequence under the sort's total order.
type argminTopK struct {
	bound, nk, width int
	desc             []bool
	tuples           []value.Value // slot*width flat
	keys             []value.Value // slot*nk flat
	seqs             []int
	heap             []int // heap of slots, worst candidate at root
	pos              []int // slot -> heap index
	n                int
	byPacked         map[uint64]int // packed tuple identity -> slot
	byBytes          map[string]int
	slotPacked       []bool // per-slot identity for eviction
	slotPK           []uint64
	slotSK           []string
	dkey             []byte
}

func newArgminTopK(bound, nk, width int, desc []bool) *argminTopK {
	return &argminTopK{
		bound: bound, nk: nk, width: width, desc: desc,
		tuples:   make([]value.Value, bound*width),
		keys:     make([]value.Value, bound*nk),
		seqs:     make([]int, bound),
		heap:     make([]int, 0, bound),
		pos:      make([]int, bound),
		byPacked: map[uint64]int{}, byBytes: map[string]int{},
		slotPacked: make([]bool, bound),
		slotPK:     make([]uint64, bound),
		slotSK:     make([]string, bound),
	}
}

// less reports whether (kbuf, seq) sorts strictly before slot's entry.
func (a *argminTopK) less(kbuf []value.Value, seq, slot int) bool {
	for k := range a.nk {
		ord := value.OrderCmp(kbuf[k], a.keys[slot*a.nk+k])
		if a.desc[k] {
			ord = -ord
		}
		if ord != 0 {
			return ord < 0
		}
	}
	return seq < a.seqs[slot]
}

// cmpSlots orders two slots under the same total order.
func (a *argminTopK) cmpSlots(s1, s2 int) int {
	if a.less(a.keys[s1*a.nk:(s1+1)*a.nk], a.seqs[s1], s2) {
		return -1
	}
	return 1
}

// offer routes one group row: an existing candidate improves in place, a
// new tuple enters when under capacity or beating the worst candidate
// (which is then evicted, membership and all), anything else is skipped.
func (a *argminTopK) offer(row []value.Value, dcols []int, kbuf []value.Value, seq int) {
	if a.bound == 0 {
		return
	}
	packed := false
	var pk uint64
	if len(dcols) == 1 {
		pk, packed = packGroupKey1(row[dcols[0]])
	}
	if !packed {
		a.dkey = a.dkey[:0]
		for _, c := range dcols {
			a.dkey = value.AppendKey(a.dkey, row[c])
		}
	}
	slot, tracked := -1, false
	if packed {
		slot, tracked = a.byPacked[pk]
	} else {
		slot, tracked = a.byBytes[string(a.dkey)]
	}
	if tracked {
		if a.less(kbuf, seq, slot) {
			copy(a.keys[slot*a.nk:(slot+1)*a.nk], kbuf)
			a.seqs[slot] = seq
			a.siftDown(a.pos[slot])
		}
		return
	}
	if a.n < a.bound {
		slot = a.n
		a.n++
		a.fillSlot(slot, row, dcols, kbuf, seq, packed, pk)
		a.heap = append(a.heap, slot)
		a.pos[slot] = len(a.heap) - 1
		a.siftUp(len(a.heap) - 1)
		return
	}
	worst := a.heap[0]
	if !a.less(kbuf, seq, worst) {
		return
	}
	if a.slotPacked[worst] {
		delete(a.byPacked, a.slotPK[worst])
	} else {
		delete(a.byBytes, a.slotSK[worst])
	}
	a.fillSlot(worst, row, dcols, kbuf, seq, packed, pk)
	a.siftDown(0)
}

func (a *argminTopK) fillSlot(slot int, row []value.Value, dcols []int, kbuf []value.Value, seq int, packed bool, pk uint64) {
	for i, c := range dcols {
		a.tuples[slot*a.width+i] = row[c]
	}
	copy(a.keys[slot*a.nk:(slot+1)*a.nk], kbuf)
	a.seqs[slot] = seq
	a.slotPacked[slot] = packed
	if packed {
		a.slotPK[slot] = pk
		a.byPacked[pk] = slot
	} else {
		sk := string(a.dkey)
		a.slotSK[slot] = sk
		a.byBytes[sk] = slot
	}
}

func (a *argminTopK) swap(i, j int) {
	a.heap[i], a.heap[j] = a.heap[j], a.heap[i]
	a.pos[a.heap[i]], a.pos[a.heap[j]] = i, j
}

func (a *argminTopK) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if a.cmpSlots(a.heap[i], a.heap[parent]) <= 0 {
			return
		}
		a.swap(i, parent)
		i = parent
	}
}

func (a *argminTopK) siftDown(i int) {
	for {
		l := 2*i + 1
		if l >= len(a.heap) {
			return
		}
		big := l
		if r := l + 1; r < len(a.heap) && a.cmpSlots(a.heap[r], a.heap[l]) > 0 {
			big = r
		}
		if a.cmpSlots(a.heap[big], a.heap[i]) <= 0 {
			return
		}
		a.swap(i, big)
		i = big
	}
}

// sorted returns the surviving tuples in final order.
func (a *argminTopK) sorted() [][]value.Value {
	idx := make([]int, a.n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, a.cmpSlots)
	out := make([][]value.Value, a.n)
	for i, s := range idx {
		out[i] = a.tuples[s*a.width : s*a.width+a.width : s*a.width+a.width]
	}
	return out
}
