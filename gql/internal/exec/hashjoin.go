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
	"github.com/freeeve/gochickpeas/internal/flatset"
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
// captured pairs (capped sub-slices of the table's append-only slabs),
// plus the intrusive link to the next row sharing its join key.
type hjRow struct {
	vals  []value.Value
	pairs []hjPair
	next  int32
}

// hjTable is one build's result, indexed by the key slot's node (expand-
// keyed joins) or the key expression's encoded value (value-keyed joins).
// Rows chain per key through hjRow.next in insertion order (heads/tails
// hold each chain's ends), and payload values and pairs pack into shared
// slabs -- the per-row map-bucket appends and slice makes the map-of-
// slices form paid were the build sink's whole allocation profile.
type hjTable struct {
	rows     []hjRow
	valSlab  []value.Value
	pairSlab []hjPair
	byKey    flatset.U64Map
	byVal    flatset.ByteMap
	heads    []int32
	tails    []int32
}

// mintChain opens an empty chain slot (the GetOrCreate create hook).
func (t *hjTable) mintChain() int {
	t.heads = append(t.heads, -1)
	t.tails = append(t.tails, -1)
	return len(t.heads) - 1
}

// link appends row idx to chain ci, preserving insertion order.
func (t *hjTable) link(ci int, idx int32) {
	if t.tails[ci] < 0 {
		t.heads[ci] = idx
	} else {
		t.rows[t.tails[ci]].next = idx
	}
	t.tails[ci] = idx
}

// headKey is chain head for an expand key; -1 when the key has no rows.
func (t *hjTable) headKey(key graph.NodeID) int32 {
	if ci, ok := t.byKey.Get(uint64(key)); ok {
		return t.heads[ci]
	}
	return -1
}

// headVal is chain head for an encoded value key; -1 when absent.
func (t *hjTable) headVal(k []byte) int32 {
	if ci, ok := t.byVal.Get(k); ok {
		return t.heads[ci]
	}
	return -1
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
	// keyProbe is the value-keyed join's outer-side key (nil for the
	// expand-keyed form); valBuf is its encoding scratch.
	keyProbe RowEval
	valBuf   []byte
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
	if hj.KeyProbe != nil {
		h.keyProbe = compileEval(ctx, hj.KeyProbe, seg.Slots)
	}
	return h
}

func (h *hashJoinSink) push(row []value.Value) bool {
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
		return true
	}
	// Cartesian probe: every build row emits per outer row (the keyless
	// disconnected join -- one bucket, the same emission protocol).
	if h.hj.Cartesian {
		copy(h.buf, row)
		return h.emitRows(t, t.headVal(nil), nil, 0, 0)
	}
	// Value-keyed probe: the outer row's key value looks the bucket up
	// directly -- no expand, no probe uniqueness (no relationship binds).
	// A null key never matches, per the consumed equality's semantics.
	if h.keyProbe != nil {
		v := h.keyProbe.Eval(h.ctx, row, h.slots)
		if v.IsNull() {
			return true
		}
		h.valBuf = value.AppendKey(h.valBuf[:0], v)
		copy(h.buf, row)
		return h.emitRows(t, t.headVal(h.valBuf), nil, 0, 0)
	}
	from, ok := row[h.hj.Probe.From].AsNode()
	if !ok {
		return true
	}
	// A reversed probe's target constraints belong to the outer endpoint
	// (the original hop's To); check them once per row.
	if h.hj.Reversed && !h.ctx.G.NodeMatcherAccepts(h.probeM, from) {
		return true
	}
	copy(h.buf, row)
	// The candidate list keeps parallel relationships (one entry each):
	// the consumed connecting expand carries per-rel multiplicity in the
	// nested rebind, so the probe must too -- deduplicating here made the
	// extraction result-visible on multigraphs. The sort is only for
	// deterministic emission order.
	h.nbuf = h.ctx.G.AppendNeighborsMatched(h.nbuf[:0], from, h.hj.Probe.Dir, h.probeRM)
	slices.Sort(h.nbuf)
	pu := h.hj.Probe.Uniq
	for _, cand := range h.nbuf {
		if !h.hj.Reversed && !h.ctx.G.NodeMatcherAccepts(h.probeM, cand) {
			continue
		}
		head := t.headKey(cand)
		if head < 0 {
			continue
		}
		var pa, pb graph.NodeID
		if pu != nil {
			pa, pb = uniqPair(h.hj.Probe.Dir, from, cand)
			if pu.Check && h.uniq.used(pu.Scope, pa, pb) {
				continue
			}
		}
		if !h.emitRows(t, head, pu, pa, pb) {
			return false
		}
	}
	return true
}

// emitRows binds each hit row's payload into the buffered outer row and
// emits it through the pair replay protocol: captured Check pairs replay
// against the outer used-pair env, captured live pairs (plus the probe
// hop's own, when there is one) push for downstream ops. Reports the
// downstream keep-going verdict; a stop still restores the pair stack.
func (h *hashJoinSink) emitRows(t *hjTable, head int32, pu *plan.RelUniq, pa, pb graph.NodeID) bool {
	for ri := head; ri >= 0; ri = t.rows[ri].next {
		r := &t.rows[ri]
		blocked := false
		for _, p := range r.pairs {
			if p.check && h.uniq.used(p.scope, p.a, p.b) {
				blocked = true
				break
			}
			// The probe hop's own pair against this row's live pairs: in
			// effective execution order the build ops precede the probe, so
			// a Check-marked probe must not reuse a relationship the row
			// already bound. (The outer env cannot catch this -- the row's
			// pairs push only after the probe's own check ran.)
			if pu != nil && pu.Check && p.live && p.scope == pu.Scope && p.a == pa && p.b == pb {
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
		more := true
		if pass {
			if h.count != nil {
				*h.count++
			}
			more = h.next.push(h.buf)
		}
		h.uniq.stack = h.uniq.stack[:mark]
		if !more {
			return false
		}
	}
	return true
}

func (h *hashJoinSink) close() { h.next.close() }

// build runs the branch chain once over a seed row carrying only the
// external slots, materializing key -> payload rows with captured pairs.
func (h *hashJoinSink) build(row []value.Value) *hjTable {
	t := &hjTable{}
	seed := make([]value.Value, h.seg.RowWidth)
	for _, s := range h.hj.ExtSlots {
		seed[s] = row[s]
	}
	benv := &uniqEnv{capture: true}
	ext := make(map[int]bool, len(h.hj.ExtSlots))
	for _, s := range h.hj.ExtSlots {
		ext[s] = true
	}
	bs := &hjBuildSink{t: t, hj: h.hj, uniq: benv}
	if h.hj.KeyBuild != nil {
		bs.keyBuild = compileEval(h.ctx, h.hj.KeyBuild, h.seg.Slots)
		bs.ctx, bs.slots = h.ctx, h.seg.Slots
	}
	var sink rowSink = bs
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
	// keyBuild is the value-keyed join's branch-side key (nil for the
	// expand-keyed form).
	keyBuild RowEval
	ctx      *eval.Ctx
	slots    map[string]int
	valBuf   []byte
}

// push always reports true: the build chain's downstream is the join
// table, not the query sink, so a stop verdict never originates here.
func (b *hjBuildSink) push(row []value.Value) bool {
	var key graph.NodeID
	if b.hj.Cartesian {
		// One bucket: the probe emits every row.
	} else if b.keyBuild != nil {
		// A null build key can never equal any probe value: drop the row.
		v := b.keyBuild.Eval(b.ctx, row, b.slots)
		if v.IsNull() {
			return true
		}
		b.valBuf = value.AppendKey(b.valBuf[:0], v)
	} else {
		var ok bool
		key, ok = row[b.hj.KeySlot].AsNode()
		if !ok {
			return true
		}
	}
	r := hjRow{next: -1}
	vo := len(b.t.valSlab)
	for _, s := range b.hj.PayloadSlots {
		b.t.valSlab = append(b.t.valSlab, row[s])
	}
	r.vals = b.t.valSlab[vo:len(b.t.valSlab):len(b.t.valSlab)]
	if n := len(b.uniq.stack); n > 0 {
		po := len(b.t.pairSlab)
		for _, k := range b.uniq.stack {
			b.t.pairSlab = append(b.t.pairSlab, hjPair{scope: k.scope, a: k.a, b: k.b, check: k.check, live: !k.dead})
		}
		r.pairs = b.t.pairSlab[po:len(b.t.pairSlab):len(b.t.pairSlab)]
	}
	idx := int32(len(b.t.rows))
	b.t.rows = append(b.t.rows, r)
	var ci int
	switch {
	case b.hj.Cartesian:
		ci = b.t.byVal.GetOrCreate(nil, b.t.mintChain)
	case b.keyBuild != nil:
		ci = b.t.byVal.GetOrCreate(b.valBuf, b.t.mintChain)
	default:
		ci = b.t.byKey.GetOrCreate(uint64(key), b.t.mintChain)
	}
	b.t.link(ci, idx)
	return true
}

func (b *hjBuildSink) close() {}
