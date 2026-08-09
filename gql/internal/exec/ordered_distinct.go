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

	"github.com/freeeve/gochickpeas/flatset"
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

	// Argmin accumulator: one entry per distinct tuple -- the tuple's
	// values (arena-backed), its best (minimum) key vector, and that
	// row's sequence (the sort's index tiebreak, so equal keys keep the
	// earlier row). Tuple identity indexes through the aggregator's own
	// key machinery -- a packable single value probes the u64 flat map,
	// everything else the flat byte map -- so the per-distinct-tuple cost
	// is amortized slab growth, not a Go-map string key per tuple.
	var idxPacked flatset.U64Map
	var idxBytes flatset.ByteMap
	tupleArena := rowArena{width: len(dcols)}
	var tuples [][]value.Value
	var keys []value.Value // n*nk flat
	var seqs []int
	kbuf := make([]value.Value, nk)
	var dkey []byte
	seq := 0
	newEntry := func(row []value.Value) int {
		tuple := tupleArena.alloc()
		for i, c := range dcols {
			tuple[i] = row[c]
		}
		tuples = append(tuples, tuple)
		keys = append(keys, kbuf...)
		seqs = append(seqs, seq)
		return len(tuples) - 1
	}
	term.agg.forEachGroup(ctx, &agg.Proj, func(row []value.Value) {
		for k := range oproj.OrderBy {
			if ci := colIdx[k]; ci >= 0 {
				kbuf[k] = row[ci]
			} else {
				kbuf[k] = eval.Eval(ctx, oproj.OrderBy[k].Expr, row, scope)
			}
		}
		before := len(tuples)
		e := -1
		if len(dcols) == 1 {
			if gk, packed := packGroupKey1(row[dcols[0]]); packed {
				e = idxPacked.GetOrCreate(gk, func() int { return newEntry(row) })
			}
		}
		if e < 0 {
			dkey = dkey[:0]
			for _, c := range dcols {
				dkey = value.AppendKey(dkey, row[c])
			}
			e = idxBytes.GetOrCreate(dkey, func() int { return newEntry(row) })
		}
		if e < before {
			// Strictly-smaller keys win; a tie keeps the earlier sequence.
			for k := range nk {
				ord := value.OrderCmp(kbuf[k], keys[e*nk+k])
				if desc[k] {
					ord = -ord
				}
				if ord < 0 {
					copy(keys[e*nk:(e+1)*nk], kbuf)
					seqs[e] = seq
					break
				} else if ord > 0 {
					break
				}
			}
		}
		seq++
	})

	// Bounded selection over the per-tuple minima under the sort's total
	// order (keys, then sequence -- sequences are unique, so cmp is total).
	idx := make([]int, len(tuples))
	for i := range idx {
		idx[i] = i
	}
	cmp := func(a, b int) int {
		ka, kb := a*nk, b*nk
		for k := range nk {
			ord := value.OrderCmp(keys[ka+k], keys[kb+k])
			if desc[k] {
				ord = -ord
			}
			if ord != 0 {
				return ord
			}
		}
		return seqs[a] - seqs[b]
	}
	if bound < len(idx) {
		idx = topKIdx(idx, bound, cmp)
	}
	slices.SortFunc(idx, cmp)
	out := make([][]value.Value, len(idx))
	for i, j := range idx {
		out[i] = tuples[j]
	}
	return paginate(out, dst.Proj.Skip, dst.Proj.Limit), 3, true
}
