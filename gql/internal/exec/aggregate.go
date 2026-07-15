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
	"math"
)

// aggState is one aggregate accumulator, allocated once per group per
// aggregate. It carries only the count/sum/avg scalars -- the kind-specific
// heavy state (a min/max extremum value.Value, a collect []value.Value) lives
// in slabs allocated on the aggregator only when those kinds are present, so
// the overwhelmingly common count/sum/avg grouping pays neither. Fields are
// ordered wide-to-narrow so the booleans pack into one tail word.
type aggState struct {
	sumI    acc128
	count   int64
	sumF    float64
	avgSum  float64
	avgN    int64
	sdMean  float64
	sdM2    float64
	kind    plan.AggKind
	isFloat bool
	any     bool
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
			s.sumI.add(i)
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
	case plan.AggStddevSamp, plan.AggStddevPop:
		// Welford: numerically stable single pass; non-numeric args skip,
		// like avg.
		if f, ok := arg.AsFloat(); ok {
			s.avgN++
			d := f - s.sdMean
			s.sdMean += d / float64(s.avgN)
			s.sdM2 += d * (f - s.sdMean)
		}
	}
	// AggMin/AggMax/AggCollect are folded on the aggregator's overflow slabs,
	// not here (their heavy state is off the per-group struct).
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
			return value.Float(s.sumF + s.sumI.float64())
		}
		// A true total outside int64 range is Null, matching the engine's
		// overflow policy (no per-row error channel) and the core
		// aggregate's nil Sum.
		if v, ok := s.sumI.int64(); ok {
			return value.Int(v)
		}
		return value.Null()
	case plan.AggAvg:
		if s.avgN == 0 {
			return value.Null()
		}
		return value.Float(s.avgSum / float64(s.avgN))
	case plan.AggStddevSamp:
		if s.avgN < 2 {
			return value.Float(0) // Neo4j's stdev: 0 on empty/single
		}
		return value.Float(math.Sqrt(s.sdM2 / float64(s.avgN-1)))
	case plan.AggStddevPop:
		if s.avgN == 0 {
			return value.Float(0)
		}
		return value.Float(math.Sqrt(s.sdM2 / float64(s.avgN)))
	}
	// AggMin/AggMax/AggCollect finalize off the aggregator's overflow slabs.
	return value.Null()
}

// Group state lives in flat stride-indexed slabs on the aggregator (keys,
// accumulators, DISTINCT sets) rather than per-group heap objects -- the
// grouping itself stays keyed on the canonical value encoding, identical
// to the Rust GroupKey set; the packed-uint64 index below is a perf-only
// representation, like the distinctSet entity-id fast path.

// distinctSet is one aggregate's DISTINCT dedup set. Node and relationship
// values -- the overwhelmingly common DISTINCT columns -- probe a compact
// entity-id set keyed on the raw u32 (the Rust engine's entity-id fast
// path), which allocates nothing per insert and stays small. Every other
// kind falls back to the canonical AppendKey byte string, exactly the prior
// behavior. The three maps are created lazily, so a uniform distinct column
// (the common case) holds exactly one.
type distinctSet struct {
	// small is the inline node-id form: most DISTINCT groups hold a
	// handful of entities, and a map per group is the dominant
	// aggregation allocation on entity-heavy groupings. Linear membership
	// over the inline array is faster than a map at this size; the first
	// overflow spills into the map form with identical semantics.
	nSmall uint8
	small  [8]uint32
	nodes  map[uint32]struct{}
	rels   map[uint32]struct{}
	other  map[string]struct{}
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
			for _, s := range d.small[:d.nSmall] {
				if s == uint32(id) {
					return false
				}
			}
			if int(d.nSmall) < len(d.small) {
				d.small[d.nSmall] = uint32(id)
				d.nSmall++
				return true
			}
			d.nodes = make(map[uint32]struct{}, 2*len(d.small))
			for _, s := range d.small[:d.nSmall] {
				d.nodes[s] = struct{}{}
			}
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

// aggregator is the single-pass group-by accumulator. Group state lives in
// flat slabs indexed by group number at strides len(groupC)/len(aggC), so
// a NEW group costs amortized slab growth instead of per-group heap
// objects; group keys that pack into a uint64 (entity ids and 62-bit ints,
// the common grouping columns) route through an integer-keyed index whose
// inserts allocate nothing, and only unpackable keys pay the byte-string
// map. A key tuple packs (or not) purely by its values, so the two maps
// never split a logical group.
type aggregator struct {
	groupC []RowEval
	aggC   []RowEval // nil entry = count(*)
	index  map[string]int
	indexI map[uint64]int

	nGroups     int
	keysChunks  [][]value.Value
	stateChunks [][]aggState
	seenChunks  [][]distinctSet   // filled only when a DISTINCT aggregate exists
	mmChunks    [][]value.Value   // min/max extrema, filled only when a min/max aggregate exists
	itemsChunks [][][]value.Value // collect lists, filled only when a collect aggregate exists
	hasDistinct bool
	hasMinMax   bool
	hasCollect  bool
	kinds       []plan.AggKind

	// keyScratch/gkScratch/dkScratch are the per-update key buffers, reused
	// so routing a row to an existing group (or a seen DISTINCT value)
	// allocates nothing.
	keyScratch []value.Value
	gkScratch  []byte
	dkScratch  []byte
}

func newAggregator(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) *aggregator {
	a := &aggregator{index: map[string]int{}, indexI: map[uint64]int{}}
	for _, gi := range proj.GroupIdx {
		a.groupC = append(a.groupC, compileEval(ctx, proj.Returns[gi].Expr, slots))
	}
	for _, ac := range proj.Aggs {
		if ac.Arg != nil {
			a.aggC = append(a.aggC, compileEval(ctx, ac.Arg, slots))
		} else {
			a.aggC = append(a.aggC, nil)
		}
		a.kinds = append(a.kinds, ac.Kind)
		if ac.Distinct {
			a.hasDistinct = true
		}
		switch ac.Kind {
		case plan.AggMin, plan.AggMax:
			a.hasMinMax = true
		case plan.AggCollect:
			a.hasCollect = true
		}
	}
	return a
}

// chunkGroups is the slab chunk size in groups: chunks allocate once and
// never move, so group state pays no growth-copy bytes and at most one
// partial chunk of waste.
const chunkGroups = 4096

// keysOf/statesOf/seenOf are a group's slab windows.
func (a *aggregator) keysOf(idx int) []value.Value {
	k := len(a.groupC)
	w := (idx % chunkGroups) * k
	return a.keysChunks[idx/chunkGroups][w : w+k]
}

func (a *aggregator) statesOf(idx int) []aggState {
	s := len(a.aggC)
	w := (idx % chunkGroups) * s
	return a.stateChunks[idx/chunkGroups][w : w+s]
}

func (a *aggregator) seenOf(idx int) []distinctSet {
	s := len(a.aggC)
	w := (idx % chunkGroups) * s
	return a.seenChunks[idx/chunkGroups][w : w+s]
}

func (a *aggregator) mmOf(idx int) []value.Value {
	s := len(a.aggC)
	w := (idx % chunkGroups) * s
	return a.mmChunks[idx/chunkGroups][w : w+s]
}

func (a *aggregator) itemsOf(idx int) [][]value.Value {
	s := len(a.aggC)
	w := (idx % chunkGroups) * s
	return a.itemsChunks[idx/chunkGroups][w : w+s]
}

// appendGroup claims the next slab windows for a new group, copying its
// key tuple in.
func (a *aggregator) appendGroup(keys []value.Value) int {
	idx := a.nGroups
	a.nGroups++
	if idx%chunkGroups == 0 {
		a.keysChunks = append(a.keysChunks, make([]value.Value, 0, chunkGroups*len(a.groupC)))
		a.stateChunks = append(a.stateChunks, make([]aggState, 0, chunkGroups*len(a.aggC)))
		if a.hasDistinct {
			a.seenChunks = append(a.seenChunks, make([]distinctSet, chunkGroups*len(a.aggC)))
		}
		if a.hasMinMax {
			a.mmChunks = append(a.mmChunks, make([]value.Value, chunkGroups*len(a.aggC)))
		}
		if a.hasCollect {
			a.itemsChunks = append(a.itemsChunks, make([][]value.Value, chunkGroups*len(a.aggC)))
		}
	}
	c := idx / chunkGroups
	a.keysChunks[c] = append(a.keysChunks[c], keys...)
	for _, k := range a.kinds {
		a.stateChunks[c] = append(a.stateChunks[c], aggState{kind: k})
	}
	return idx
}

// packGroupKey packs a group-key tuple into a uint64: a single entity id
// or 62-bit int, or a pair of entity ids below 2^30. Packing is a pure
// function of the values, and the 2-bit shape tag keeps the int, node,
// rel, and pair key spaces disjoint (mirroring AppendKey's kind tags).
func packGroupKey(keys []value.Value) (uint64, bool) {
	switch len(keys) {
	case 1:
		v := keys[0]
		switch v.Kind() {
		case value.KindInt:
			i, _ := v.AsInt()
			if i < -(1<<61) || i >= 1<<61 {
				return 0, false
			}
			return 0<<62 | uint64(i)&(1<<62-1), true
		case value.KindNode:
			id, _ := v.AsNode()
			return 1<<62 | uint64(uint32(id)), true
		case value.KindRel:
			pos, _ := v.AsRel()
			return 2<<62 | uint64(uint32(pos)), true
		}
	case 2:
		e1, ok1 := packedEntity30(keys[0])
		e2, ok2 := packedEntity30(keys[1])
		if ok1 && ok2 {
			return 3<<62 | e1<<31 | e2, true
		}
	}
	return 0, false
}

// packedEntity30 packs a node/rel id below 2^30 with its kind bit into 31
// bits, for the pair form of packGroupKey.
func packedEntity30(v value.Value) (uint64, bool) {
	var id uint64
	var kind uint64
	switch v.Kind() {
	case value.KindNode:
		n, _ := v.AsNode()
		id = uint64(uint32(n))
	case value.KindRel:
		p, _ := v.AsRel()
		id, kind = uint64(uint32(p)), 1
	default:
		return 0, false
	}
	if id >= 1<<30 {
		return 0, false
	}
	return kind<<30 | id, true
}

// groupIdx routes a key tuple to its group's slab index, creating the
// group on first sight.
func (a *aggregator) groupIdx(keys []value.Value) int {
	if gk64, packed := packGroupKey(keys); packed {
		idx, hit := a.indexI[gk64]
		if !hit {
			idx = a.appendGroup(keys)
			a.indexI[gk64] = idx
		}
		return idx
	}
	gk := a.gkScratch[:0]
	for _, v := range keys {
		gk = value.AppendKey(gk, v)
	}
	a.gkScratch = gk
	idx, hit := a.index[string(gk)]
	if !hit {
		idx = a.appendGroup(keys)
		a.index[string(gk)] = idx
	}
	return idx
}

// update routes one matched row into its group.
func (a *aggregator) update(ctx *eval.Ctx, m []value.Value, proj *plan.ProjPlan, slots map[string]int) {
	a.keyScratch = a.keyScratch[:0]
	for _, c := range a.groupC {
		a.keyScratch = append(a.keyScratch, c.Eval(ctx, m, slots))
	}
	idx := a.groupIdx(a.keyScratch)
	states := a.statesOf(idx)
	var seen []distinctSet
	if a.hasDistinct {
		seen = a.seenOf(idx)
	}
	var mm []value.Value
	if a.hasMinMax {
		mm = a.mmOf(idx)
	}
	var items [][]value.Value
	if a.hasCollect {
		items = a.itemsOf(idx)
	}
	for j := range proj.Aggs {
		var arg value.Value
		present := a.aggC[j] != nil
		if present {
			arg = a.aggC[j].Eval(ctx, m, slots)
		}
		if proj.Aggs[j].Distinct && present && !arg.IsNull() {
			if !seen[j].add(arg, &a.dkScratch) {
				continue
			}
		}
		switch a.kinds[j] {
		case plan.AggMin, plan.AggMax:
			// The extremum lives on the mm slab; a null slot is the
			// uninitialized sentinel (a min/max arg is never null, so the
			// first non-null arg always seeds it, matching the prior hasMM
			// flag exactly).
			if arg.IsNull() {
				continue
			}
			if mm[j].IsNull() {
				mm[j] = arg
			} else if c, ok := value.Compare(arg, mm[j]); ok &&
				((a.kinds[j] == plan.AggMin && c < 0) || (a.kinds[j] == plan.AggMax && c > 0)) {
				mm[j] = arg
			}
		case plan.AggCollect:
			if !arg.IsNull() {
				items[j] = append(items[j], arg)
			}
		default:
			states[j].update(arg, present)
		}
	}
}

// finalize emits one row per group (a zeroed row for a keyless aggregate
// over no input), applies the nested-aggregate scalar wrappers over the
// hidden slots, then orders and paginates.
func (a *aggregator) finalize(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) [][]value.Value {
	if a.nGroups == 0 && len(proj.GroupIdx) == 0 {
		a.appendGroup(nil)
	}
	nCols := len(proj.Returns)
	// Wrappers read the hidden accumulator slots as __agg{k} and the
	// grouping-key columns by name (the Rust engine's tasks/150); both are
	// filled before the wrappers run.
	postSlots := make(map[string]int, proj.NHidden+len(proj.GroupIdx))
	for k := 0; k < proj.NHidden; k++ {
		postSlots[hiddenAggName(k)] = nCols + k
	}
	for _, gi := range proj.GroupIdx {
		if _, ok := postSlots[proj.Returns[gi].Name]; !ok {
			postSlots[proj.Returns[gi].Name] = gi
		}
	}
	postC := make([]RowEval, len(proj.Post))
	for i, p := range proj.Post {
		postC[i] = compileEval(ctx, p.Expr, postSlots)
	}
	// One arena backs every output row instead of a make per group: a
	// grouping over a million groups then pays one large allocation plus its
	// row-header slice, not a million small ones. Each row is a stride
	// window (nCols visible columns + the hidden accumulator slots the
	// post-wrappers read); only the visible prefix is published.
	stride := nCols + proj.NHidden
	arena := make([]value.Value, a.nGroups*stride)
	out := make([][]value.Value, 0, a.nGroups)
	for idx := 0; idx < a.nGroups; idx++ {
		row := arena[idx*stride : idx*stride+stride : idx*stride+stride]
		keys := a.keysOf(idx)
		for k, gi := range proj.GroupIdx {
			row[gi] = keys[k]
		}
		states := a.statesOf(idx)
		var mm []value.Value
		if a.hasMinMax {
			mm = a.mmOf(idx)
		}
		var items [][]value.Value
		if a.hasCollect {
			items = a.itemsOf(idx)
		}
		for j := range proj.Aggs {
			switch a.kinds[j] {
			case plan.AggMin, plan.AggMax:
				// A null slot means no non-null arg was seen -> Null.
				row[proj.Aggs[j].OutIdx] = mm[j]
			case plan.AggCollect:
				row[proj.Aggs[j].OutIdx] = value.List(items[j])
			default:
				row[proj.Aggs[j].OutIdx] = states[j].finalize()
			}
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
