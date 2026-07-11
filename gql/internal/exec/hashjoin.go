// Hash-join stage runner: build the independent branch once per distinct
// external-slot tuple (an isolated capture-mode uniqueness env records
// each row's relationship pairs), then per outer row expand the probe hop
// and emit the key-matching payload rows -- replaying each payload's
// captured Check pairs against the outer used-pair env and pushing its
// live pairs for downstream ops, so the emitted multiset is exactly what
// the original nested execution produced.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// hjPair is one captured relationship-uniqueness pair of a payload row:
// check pairs replay against the outer env at probe time, live pairs push
// onto it while downstream ops run.
type hjPair struct {
	scope       uint32
	a, b        graph.NodeID
	check, live bool
}

// hjRow is one materialized build row: its payload-slot values and its
// captured pairs.
type hjRow struct {
	vals  []value.Value
	pairs []hjPair
}

// hjTable is one build's result, indexed by the key slot's node.
type hjTable struct {
	rows  []hjRow
	byKey map[graph.NodeID][]int32
}

// hashJoinSink is the stage's row sink.
type hashJoinSink struct {
	ctx     *eval.Ctx
	hj      *plan.HashJoinStage
	seg     *plan.Segment
	slots   map[string]int
	buf     []value.Value
	next    rowSink
	uniq    *uniqEnv
	probeM  *graph.NodeMatcher
	probeRM *graph.RelMatcher
	where   []RowEval
	tables  map[string]*hjTable
	keyBuf  []byte
	nbuf    []graph.NodeID
	count   *uint64
}

func newHashJoinSink(ctx *eval.Ctx, seg *plan.Segment, hj *plan.HashJoinStage, next rowSink, uniq *uniqEnv, count *uint64) *hashJoinSink {
	props := make([]graph.PropSpec, len(hj.Probe.Props))
	for i, p := range hj.Probe.Props {
		props[i] = graph.PropSpec{Key: p.Key, Val: eval.LitValue(ctx, p.Val)}
	}
	h := &hashJoinSink{
		ctx: ctx, hj: hj, seg: seg, slots: seg.Slots,
		buf: make([]value.Value, seg.RowWidth), next: next, uniq: uniq,
		probeM:  ctx.G.CompileNodeMatcher(hj.Probe.Labels, props),
		probeRM: ctx.G.CompileRelMatcher(hj.Probe.Types),
		tables:  map[string]*hjTable{},
		count:   count,
	}
	if hj.Where != nil {
		var conjs []ast.Expr
		plan.SplitAnd(hj.Where, &conjs)
		for _, c := range conjs {
			h.where = append(h.where, compileEval(ctx, c, seg.Slots))
		}
	}
	return h
}

func (h *hashJoinSink) push(row []value.Value) {
	k := h.keyBuf[:0]
	for _, s := range h.hj.ExtSlots {
		k = value.AppendKey(k, row[s])
	}
	h.keyBuf = k
	t, ok := h.tables[string(k)]
	if !ok {
		t = h.build(row)
		h.tables[string(k)] = t
	}
	if len(t.rows) == 0 {
		return
	}
	from, ok := row[h.hj.Probe.From].AsNode()
	if !ok {
		return
	}
	// A reversed probe's target constraints belong to the outer endpoint
	// (the original hop's To); check them once per row.
	if h.hj.Reversed && !h.ctx.G.NodeMatcherAccepts(h.probeM, from) {
		return
	}
	copy(h.buf, row)
	// The candidate set is deduplicated: the consumed connecting expand
	// was a bound-target existence check, which collapses parallel
	// relationships (the engine's documented multigraph deviation).
	h.nbuf = h.ctx.G.AppendNeighborsMatched(h.nbuf[:0], from, h.hj.Probe.Dir, h.probeRM)
	slices.Sort(h.nbuf)
	h.nbuf = slices.Compact(h.nbuf)
	pu := h.hj.Probe.Uniq
	for _, cand := range h.nbuf {
		if !h.hj.Reversed && !h.ctx.G.NodeMatcherAccepts(h.probeM, cand) {
			continue
		}
		idxs := t.byKey[cand]
		if len(idxs) == 0 {
			continue
		}
		var pa, pb graph.NodeID
		if pu != nil {
			pa, pb = uniqPair(h.hj.Probe.Dir, from, cand)
			if pu.Check && h.uniq.used(pu.Scope, pa, pb) {
				continue
			}
		}
		for _, ri := range idxs {
			r := &t.rows[ri]
			blocked := false
			for _, p := range r.pairs {
				if p.check && h.uniq.used(p.scope, p.a, p.b) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			for i, s := range h.hj.PayloadSlots {
				h.buf[s] = r.vals[i]
			}
			mark := len(h.uniq.stack)
			if pu != nil && pu.Contribute {
				h.uniq.stack = append(h.uniq.stack, uniqKey{scope: pu.Scope, a: pa, b: pb, check: pu.Check})
			}
			for _, p := range r.pairs {
				if p.live {
					h.uniq.stack = append(h.uniq.stack, uniqKey{scope: p.scope, a: p.a, b: p.b, check: p.check})
				}
			}
			pass := true
			for _, w := range h.where {
				if !w.Eval(h.ctx, h.buf, h.slots).IsTruthy() {
					pass = false
					break
				}
			}
			if pass {
				if h.count != nil {
					*h.count++
				}
				h.next.push(h.buf)
			}
			h.uniq.stack = h.uniq.stack[:mark]
		}
	}
}

func (h *hashJoinSink) close() { h.next.close() }

// build runs the branch chain once over a seed row carrying only the
// external slots, materializing key -> payload rows with captured pairs.
func (h *hashJoinSink) build(row []value.Value) *hjTable {
	t := &hjTable{byKey: map[graph.NodeID][]int32{}}
	seed := make([]value.Value, h.seg.RowWidth)
	for _, s := range h.hj.ExtSlots {
		seed[s] = row[s]
	}
	benv := &uniqEnv{capture: true}
	ext := make(map[int]bool, len(h.hj.ExtSlots))
	for _, s := range h.hj.ExtSlots {
		ext[s] = true
	}
	var sink rowSink = &hjBuildSink{t: t, hj: h.hj, uniq: benv}
	for i := len(h.hj.Build) - 1; i >= 0; i-- {
		sink = buildStageSink(h.ctx, h.seg, h.hj.Build[i], sink, func(s int) bool { return ext[s] }, seed, nil, benv)
	}
	sink.push(seed)
	sink.close()
	return t
}

// hjBuildSink materializes the build chain's rows: payload values plus a
// snapshot of the capture env's stack (the pairs bound along the current
// DFS path -- exactly this row's own).
type hjBuildSink struct {
	t    *hjTable
	hj   *plan.HashJoinStage
	uniq *uniqEnv
}

func (b *hjBuildSink) push(row []value.Value) {
	key, ok := row[b.hj.KeySlot].AsNode()
	if !ok {
		return
	}
	r := hjRow{vals: make([]value.Value, len(b.hj.PayloadSlots))}
	for i, s := range b.hj.PayloadSlots {
		r.vals[i] = row[s]
	}
	if n := len(b.uniq.stack); n > 0 {
		r.pairs = make([]hjPair, n)
		for i, k := range b.uniq.stack {
			r.pairs[i] = hjPair{scope: k.scope, a: k.a, b: k.b, check: k.check, live: !k.dead}
		}
	}
	idx := int32(len(b.t.rows))
	b.t.rows = append(b.t.rows, r)
	b.t.byKey[key] = append(b.t.byKey[key], idx)
}

func (b *hjBuildSink) close() {}
