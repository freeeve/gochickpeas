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

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
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
	seen    *flatset.ByteSet
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
	// kGate marks the payload-gated top-k path: every ORDER BY key either
	// is an output column or evaluates against the matched row alone
	// (kRowEval, the RETURN m ... ORDER BY m.x identity family), and no
	// DISTINCT dedups on the full row -- so the keys can evaluate first
	// and the heap can refuse a candidate BEFORE its remaining columns are
	// built (most candidates die on one comparison once the heap fills).
	kGate    bool
	kRowEval []RowEval
	// kTyped is the typed prefilter over the gated path: when every
	// ORDER BY key is a bare i64-column property read of a node slot,
	// a full heap pre-tests each row against a typed shadow of the
	// root's key tuple with float64 compares (mirroring OrderCmp's
	// Int-tier totalCmpF64 exactly, ties included) before any boxed
	// evaluation. A failed typed read (null slot, absent property) or
	// an unshadowable root falls back to the boxed flow per row.
	kTyped    []func(row []value.Value) (float64, bool)
	kTypedBuf []float64
}

// topkPayloadBuilds counts full projected payload constructions on the
// gated top-k path -- the load-independent oracle: ORDER BY v ASC LIMIT k
// over ascending input builds exactly k payloads, not one per row.
// disableTopkGate pins differential tests to the unguarded
// build-then-offer flow (the reference the gate must match exactly).
var (
	topkPayloadBuilds int
	disableTopkGate   bool
)

// typedSinkRejects counts rows refused by the typed prefilter alone --
// the engagement oracle: a full heap over a classifiable key set must
// reject through int-path compares, not boxed OrderCmp.
// disableTypedSink pins differential tests to the boxed gated flow.
var (
	typedSinkRejects int
	disableTypedSink bool
)

// typedKeyReader classifies one ORDER BY key expression as a bare
// i64-column property read of a node slot, returning a reader that
// yields the column value through the same float64 conversion
// OrderCmp's Int tier orders by. ok=false declines (any other shape,
// dtype, or a missing column -- whose boxed reads would be Null).
func typedKeyReader(ctx *eval.Ctx, e ast.Expr, slots map[string]int) (func(row []value.Value) (float64, bool), bool) {
	prop, ok := e.(*ast.Prop)
	if !ok {
		return nil, false
	}
	slot, ok := slots[prop.Var]
	if !ok {
		return nil, false
	}
	native, ok := ctx.G.(graph.Native)
	if !ok {
		return nil, false
	}
	col, ok := native.Snapshot().ColIndexed(prop.Key)
	if !ok || col.Dtype() != chickpeas.DtypeI64 {
		return nil, false
	}
	r := col.I64()
	return func(row []value.Value) (float64, bool) {
		id, ok := row[slot].AsNode()
		if !ok {
			return 0, false
		}
		v, ok := r.Get(uint32(id))
		if !ok {
			return 0, false
		}
		return float64(v), true
	}, true
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
			p.seen = &flatset.ByteSet{}
		}
	}
	if bound := orderBound(proj); bound >= 0 {
		// Retention is bounded, so size arena chunks to it (plus eviction
		// turnover) instead of the default sweep-sized chunk -- a
		// LIMIT-bounded sink otherwise pays a near-empty 16K-value chunk
		// per arena (the aggregate finalize precedent).
		p.oArena.chunkValues = min(arenaChunkValues, max(64, 2*bound)*len(proj.Returns))
		if p.needM {
			p.mArena.chunkValues = min(arenaChunkValues, max(64, 2*bound)*width)
		}
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
			p.kGate = !proj.Distinct && !disableTopkGate
			for k := range proj.OrderBy {
				if p.kColIdx[k] < 0 {
					// Not an output column, but still gateable when the
					// key's references all resolve in the matched row's
					// scope (RETURN m ... ORDER BY m.x and friends): it
					// then evaluates pre-build, exactly like a column key.
					if semantics.CheckRefs(proj.OrderBy[k].Expr, slots) == nil {
						if p.kRowEval == nil {
							p.kRowEval = make([]RowEval, nk)
						}
						p.kRowEval[k] = compileEval(ctx, proj.OrderBy[k].Expr, slots)
						continue
					}
					p.kGate = false // key needs the built row
					break
				}
			}
			if p.kGate && !disableTypedSink {
				typed := make([]func([]value.Value) (float64, bool), len(proj.OrderBy))
				allTyped := true
				for k := range proj.OrderBy {
					kexpr := proj.OrderBy[k].Expr
					if idx := p.kColIdx[k]; idx >= 0 {
						kexpr = proj.Returns[idx].Expr
					}
					rd, ok := typedKeyReader(ctx, kexpr, slots)
					if !ok {
						allTyped = false
						break
					}
					typed[k] = rd
				}
				if allTyped {
					p.kTyped = typed
					p.kTypedBuf = make([]float64, len(proj.OrderBy))
					p.topk.typedArmed = true
					p.topk.thr = make([]float64, p.topk.nk)
				}
			}
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

func (p *projSink) push(row []value.Value) bool {
	if p.limitCap >= 0 && len(p.outs) >= p.limitCap {
		// Arrival order is output order (no ORDER BY), so nothing a
		// producer could still emit can surface: stop the walk.
		return false
	}
	// Gated top-k: evaluate only the key columns and ask the heap first;
	// a refused candidate never builds its remaining columns (nor touches
	// the arena). Skipping is byte-identical to offer-then-pop (see
	// wouldAccept), so the surviving rows match the unguarded path
	// exactly.
	if p.kGate {
		// Typed prefilter: a full heap with a valid typed shadow refuses
		// most rows on raw column reads and float64 compares -- no boxed
		// key evaluation, no OrderCmp. Any typed-read miss falls through
		// to the boxed flow for that row; skipping is decision-identical
		// because the shadow holds exactly the root's key ordering and
		// ties lose on both paths.
		if p.kTyped != nil && p.topk.n == p.topk.bound && p.topk.thrValid {
			typedOK := true
			for k, rd := range p.kTyped {
				v, ok := rd(row)
				if !ok {
					typedOK = false
					break
				}
				p.kTypedBuf[k] = v
			}
			if typedOK && p.topk.typedReject(p.kTypedBuf) {
				typedSinkRejects++
				p.topk.seq++
				return true
			}
		}
		for k := range p.proj.OrderBy {
			if idx := p.kColIdx[k]; idx >= 0 {
				p.kBuf[k] = p.returns[idx].Eval(p.ctx, row, p.slots)
			} else {
				p.kBuf[k] = p.kRowEval[k].Eval(p.ctx, row, p.slots)
			}
		}
		if !p.topk.wouldAccept(p.kBuf) {
			p.topk.seq++ // rejected offers still order future arrivals
			return true
		}
		topkPayloadBuilds++
		out := p.oArena.alloc()
		for k := range p.proj.OrderBy {
			if idx := p.kColIdx[k]; idx >= 0 {
				out[idx] = p.kBuf[k]
			}
		}
		for i, c := range p.returns {
			if !p.kGateCol(i) {
				out[i] = c.Eval(p.ctx, row, p.slots)
			}
		}
		if !p.topk.offer(p.pushKeys(out, row), out) {
			p.oArena.rollback()
		}
		return true
	}
	out := p.oArena.alloc()
	for i, c := range p.returns {
		out[i] = c.Eval(p.ctx, row, p.slots)
	}
	if p.seenOne != nil {
		if !p.seenOne.add(out[0], &p.key) {
			p.oArena.rollback()
			return true
		}
	} else if p.seen != nil {
		p.key = p.key[:0]
		for _, v := range out {
			p.key = value.AppendKey(p.key, v)
		}
		if !p.seen.Add(p.key) {
			p.oArena.rollback()
			return true
		}
	}
	if p.topk != nil {
		// Keys evaluate here, against the live matched row -- so the
		// matched row never needs retaining at all on this path.
		if !p.topk.offer(p.pushKeys(out, row), out) {
			p.oArena.rollback()
		}
		return true
	}
	p.outs = append(p.outs, out)
	if p.needM {
		p.ms = append(p.ms, p.mArena.copyRow(row))
	}
	// Reaching the unordered cap exactly now also stops the walk (a
	// deferred false would cost one more full candidate).
	return p.limitCap < 0 || len(p.outs) < p.limitCap
}

// kGateCol reports whether output column i is one of the gated path's
// already-evaluated key columns.
func (p *projSink) kGateCol(i int) bool {
	for k := range p.kColIdx {
		if p.kColIdx[k] == i {
			return true
		}
	}
	return false
}

func (p *projSink) close() {}

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
