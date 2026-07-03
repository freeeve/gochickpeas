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
	"sort"

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
	seen   map[string]struct{}
	key    []byte
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
		p.seen = map[string]struct{}{}
	}
	return p
}

func (p *projSink) push(row []value.Value) {
	out := p.oArena.alloc()
	for i, c := range p.returns {
		out[i] = c.Eval(p.ctx, row, p.slots)
	}
	if p.seen != nil {
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
		mAt := func(int) []value.Value { return nil }
		if p.needM {
			mAt = func(i int) []value.Value { return p.ms[i] }
		}
		idx := make([]int, len(outs))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return cmpOrder(p.ctx, p.proj, p.slots, mAt(idx[a]), outs[idx[a]], mAt(idx[b]), outs[idx[b]]) < 0
		})
		sorted := make([][]value.Value, len(outs))
		for i, j := range idx {
			sorted[i] = outs[j]
		}
		outs = sorted
	}
	return paginate(outs, p.proj.Skip, p.proj.Limit)
}

// cmpOrder compares two (matched, projected) row pairs per the ORDER BY
// items.
func cmpOrder(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int, am, ao, bm, bo []value.Value) int {
	for i := range proj.OrderBy {
		s := &proj.OrderBy[i]
		ka := orderKey(ctx, s, am, ao, proj, slots)
		kb := orderKey(ctx, s, bm, bo, proj, slots)
		ord := value.OrderCmp(ka, kb)
		if s.Desc {
			ord = -ord
		}
		if ord != 0 {
			return ord
		}
	}
	return 0
}

// orderKey resolves one ORDER BY key: a key that is a whole projected
// column (its alias or exact expression) reads the projected value
// directly; otherwise it evaluates over the incoming row extended with the
// projected columns, so it can combine projection aliases (which shadow
// same-named incoming variables) with incoming variables.
func orderKey(ctx *eval.Ctx, s *ast.SortItem, matched, out []value.Value, proj *plan.ProjPlan, slots map[string]int) value.Value {
	if idx := plan.OrderColIndex(s.Expr, proj.Columns, proj.Returns); idx >= 0 {
		return out[idx]
	}
	row := make([]value.Value, 0, len(matched)+len(out))
	row = append(row, matched...)
	row = append(row, out...)
	scope := make(map[string]int, len(slots)+len(proj.Columns))
	maps.Copy(scope, slots)
	for i, c := range proj.Columns {
		scope[c] = len(matched) + i
	}
	return eval.Eval(ctx, s.Expr, row, scope)
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
