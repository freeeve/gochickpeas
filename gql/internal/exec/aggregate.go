// The group-by aggregator: rows route to their group's accumulators
// (implicit group-by-the-non-aggregate-keys), then one output row per
// group finalizes with ordering/pagination. count/sum/avg/min/max/collect
// with per-aggregate DISTINCT; nested-aggregate scalar wrappers read
// hidden accumulator slots and truncate them after. Encounter order of
// groups is preserved (observable in unordered results).
package exec

import (
	"strconv"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// aggState is one aggregate accumulator.
type aggState struct {
	kind    plan.AggKind
	count   int64
	sumI    int64
	sumF    float64
	isFloat bool
	any     bool
	avgSum  float64
	avgN    int64
	minmax  value.Value
	hasMM   bool
	items   []value.Value
}

// update folds one argument in (arg absent means count(*)).
func (s *aggState) update(arg value.Value, present bool) {
	switch s.kind {
	case plan.AggCount:
		if !present || !arg.IsNull() {
			s.count++
		}
	case plan.AggSum:
		if i, ok := arg.AsInt(); ok {
			s.sumI += i
			s.any = true
		} else if arg.Kind() == value.KindFloat {
			f, _ := arg.AsFloat()
			s.sumF += f
			s.isFloat = true
			s.any = true
		}
	case plan.AggAvg:
		if f, ok := arg.AsFloat(); ok {
			s.avgSum += f
			s.avgN++
		}
	case plan.AggMin, plan.AggMax:
		if arg.IsNull() {
			return
		}
		if !s.hasMM {
			s.minmax, s.hasMM = arg, true
			return
		}
		if c, ok := value.Compare(arg, s.minmax); ok &&
			((s.kind == plan.AggMin && c < 0) || (s.kind == plan.AggMax && c > 0)) {
			s.minmax = arg
		}
	case plan.AggCollect:
		if !arg.IsNull() {
			s.items = append(s.items, arg)
		}
	}
}

// finalize emits the accumulator's value.
func (s *aggState) finalize() value.Value {
	switch s.kind {
	case plan.AggCount:
		return value.Int(s.count)
	case plan.AggSum:
		switch {
		case !s.any:
			return value.Int(0)
		case s.isFloat:
			return value.Float(s.sumF + float64(s.sumI))
		}
		return value.Int(s.sumI)
	case plan.AggAvg:
		if s.avgN == 0 {
			return value.Null()
		}
		return value.Float(s.avgSum / float64(s.avgN))
	case plan.AggMin, plan.AggMax:
		if !s.hasMM {
			return value.Null()
		}
		return s.minmax
	default: // plan.AggCollect
		return value.List(s.items)
	}
}

// aggGroup is one group's key values, accumulators, and per-aggregate
// DISTINCT dedup sets (keyed on the canonical value encoding -- identical
// grouping to the Rust GroupKey set; the Rust entity-id fast path is a
// perf-only representation).
type aggGroup struct {
	keys   []value.Value
	states []aggState
	seen   []distinctSet
}

// distinctSet is one aggregate's DISTINCT dedup set. Node and relationship
// values -- the overwhelmingly common DISTINCT columns -- probe a compact
// entity-id set keyed on the raw u32 (the Rust engine's entity-id fast
// path), which allocates nothing per insert and stays small. Every other
// kind falls back to the canonical AppendKey byte string, exactly the prior
// behavior. The three maps are created lazily, so a uniform distinct column
// (the common case) holds exactly one.
type distinctSet struct {
	nodes map[uint32]struct{}
	rels  map[uint32]struct{}
	other map[string]struct{}
}

// add reports whether v is newly seen (and records it), reusing scratch for
// the byte-string fallback encoding. Node/rel identity is exact u32
// equality, matching AppendKey's tagNode/tagRel + u32 encoding; the two
// entity kinds key separate maps so a node and a relationship of equal id
// never conflate.
func (d *distinctSet) add(v value.Value, scratch *[]byte) bool {
	switch v.Kind() {
	case value.KindNode:
		id, _ := v.AsNode()
		if d.nodes == nil {
			d.nodes = map[uint32]struct{}{}
		}
		if _, dup := d.nodes[uint32(id)]; dup {
			return false
		}
		d.nodes[uint32(id)] = struct{}{}
		return true
	case value.KindRel:
		pos, _ := v.AsRel()
		if d.rels == nil {
			d.rels = map[uint32]struct{}{}
		}
		if _, dup := d.rels[uint32(pos)]; dup {
			return false
		}
		d.rels[uint32(pos)] = struct{}{}
		return true
	}
	*scratch = value.AppendKey((*scratch)[:0], v)
	if d.other == nil {
		d.other = map[string]struct{}{}
	}
	if _, dup := d.other[string(*scratch)]; dup {
		return false
	}
	d.other[string(*scratch)] = struct{}{}
	return true
}

// aggregator is the single-pass group-by accumulator.
type aggregator struct {
	groupC []RowEval
	aggC   []RowEval // nil entry = count(*)
	index  map[string]int
	groups []*aggGroup
	// keyScratch/gkScratch/dkScratch are the per-update key buffers, reused
	// so routing a row to an existing group (or a seen DISTINCT value)
	// allocates nothing.
	keyScratch []value.Value
	gkScratch  []byte
	dkScratch  []byte
}

func newAggregator(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) *aggregator {
	a := &aggregator{index: map[string]int{}}
	for _, gi := range proj.GroupIdx {
		a.groupC = append(a.groupC, compileEval(ctx, proj.Returns[gi].Expr, slots))
	}
	for _, ac := range proj.Aggs {
		if ac.Arg != nil {
			a.aggC = append(a.aggC, compileEval(ctx, ac.Arg, slots))
		} else {
			a.aggC = append(a.aggC, nil)
		}
	}
	return a
}

func freshGroup(proj *plan.ProjPlan, keys []value.Value) *aggGroup {
	g := &aggGroup{
		keys:   keys,
		states: make([]aggState, len(proj.Aggs)),
		seen:   make([]distinctSet, len(proj.Aggs)),
	}
	for i, ac := range proj.Aggs {
		g.states[i].kind = ac.Kind
	}
	return g
}

// update routes one matched row into its group.
func (a *aggregator) update(ctx *eval.Ctx, m []value.Value, proj *plan.ProjPlan, slots map[string]int) {
	a.keyScratch = a.keyScratch[:0]
	gk := a.gkScratch[:0]
	for _, c := range a.groupC {
		v := c.Eval(ctx, m, slots)
		a.keyScratch = append(a.keyScratch, v)
		gk = value.AppendKey(gk, v)
	}
	a.gkScratch = gk
	idx, ok := a.index[string(gk)]
	if !ok {
		idx = len(a.groups)
		keys := make([]value.Value, len(a.keyScratch))
		copy(keys, a.keyScratch)
		a.groups = append(a.groups, freshGroup(proj, keys))
		a.index[string(gk)] = idx
	}
	grp := a.groups[idx]
	for j := range proj.Aggs {
		var arg value.Value
		present := a.aggC[j] != nil
		if present {
			arg = a.aggC[j].Eval(ctx, m, slots)
		}
		if proj.Aggs[j].Distinct && present && !arg.IsNull() {
			if !grp.seen[j].add(arg, &a.dkScratch) {
				continue
			}
		}
		grp.states[j].update(arg, present)
	}
}

// finalize emits one row per group (a zeroed row for a keyless aggregate
// over no input), applies the nested-aggregate scalar wrappers over the
// hidden slots, then orders and paginates.
func (a *aggregator) finalize(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) [][]value.Value {
	if len(a.groups) == 0 && len(proj.GroupIdx) == 0 {
		a.groups = append(a.groups, freshGroup(proj, nil))
	}
	nCols := len(proj.Returns)
	postSlots := make(map[string]int, proj.NHidden)
	for k := 0; k < proj.NHidden; k++ {
		postSlots[hiddenAggName(k)] = nCols + k
	}
	postC := make([]RowEval, len(proj.Post))
	for i, p := range proj.Post {
		postC[i] = compileEval(ctx, p.Expr, postSlots)
	}
	out := make([][]value.Value, 0, len(a.groups))
	for _, grp := range a.groups {
		row := make([]value.Value, nCols+proj.NHidden)
		for k, gi := range proj.GroupIdx {
			row[gi] = grp.keys[k]
		}
		for j := range proj.Aggs {
			row[proj.Aggs[j].OutIdx] = grp.states[j].finalize()
		}
		for i, p := range proj.Post {
			row[p.Col] = postC[i].Eval(ctx, row, postSlots)
		}
		out = append(out, row[:nCols])
	}
	if len(proj.OrderBy) > 0 {
		out = sortRowsByOrder(ctx, proj, slots, func(int) []value.Value { return nil }, 0, out)
	}
	return paginate(out, proj.Skip, proj.Limit)
}

// hiddenAggName is the rewritten hidden-slot variable a post-aggregation
// wrapper reads (must match the planner's __agg{k} rewrite).
func hiddenAggName(k int) string {
	return "__agg" + strconv.Itoa(k)
}

// aggSink is the aggregated terminal sink: it streams matched rows into
// the group accumulator, so only per-group state is retained.
type aggSink struct {
	ctx   *eval.Ctx
	agg   *aggregator
	proj  *plan.ProjPlan
	slots map[string]int
}

func newAggSink(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) *aggSink {
	return &aggSink{ctx: ctx, agg: newAggregator(ctx, proj, slots), proj: proj, slots: slots}
}

func (a *aggSink) push(row []value.Value) {
	a.agg.update(a.ctx, row, a.proj, a.slots)
}

func (a *aggSink) close() {}

func (a *aggSink) finalize() [][]value.Value {
	return a.agg.finalize(a.ctx, a.proj, a.slots)
}
