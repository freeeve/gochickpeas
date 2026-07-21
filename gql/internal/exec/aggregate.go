// The group-by aggregator: rows route to their group's accumulators
// (implicit group-by-the-non-aggregate-keys), then one output row per
// group finalizes with ordering/pagination. count/sum/avg/min/max/collect
// with per-aggregate DISTINCT; nested-aggregate scalar wrappers read
// hidden accumulator slots and truncate them after. Encounter order of
// groups is preserved (observable in unordered results).
package exec

import (
	"math"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
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
	// small is the inline entity-id form: most DISTINCT groups hold a
	// handful of entities, and a map per group is the dominant
	// aggregation allocation on entity-heavy groupings. Linear membership
	// over the inline array is faster than a map at this size; the first
	// overflow spills into the map form with identical semantics. The
	// first-seen entity kind claims the array (smRel marks a rel claim) --
	// a mixed node/rel column, which no plan produces, sends the other
	// kind straight to its map so the two id spaces never conflate.
	nSmall uint8
	smRel  bool
	small  [8]uint32
	nodes  flatset.U32Set
	rels   flatset.U32Set
	other  flatset.ByteSet
}

// add reports whether v is newly seen (and records it), reusing scratch for
// the byte-string fallback encoding. Node/rel identity is exact u32
// equality, matching AppendKey's tagNode/tagRel + u32 encoding; the two
// entity kinds key separate stores so a node and a relationship of equal
// id never conflate.
func (d *distinctSet) add(v value.Value, scratch *[]byte) bool {
	switch v.Kind() {
	case value.KindNode:
		id, _ := v.AsNode()
		return d.addEntity(uint32(id), false, &d.nodes)
	case value.KindRel:
		pos, _ := v.AsRel()
		return d.addEntity(uint32(pos), true, &d.rels)
	}
	*scratch = value.AppendKey((*scratch)[:0], v)
	return d.other.Add(*scratch)
}

// addEntity dedups one entity id through the inline array (when this kind
// holds the claim) or the kind's probe set, spilling the inline ids into
// the set on overflow.
func (d *distinctSet) addEntity(id uint32, isRel bool, m *flatset.U32Set) bool {
	if !m.Built() {
		if d.nSmall == 0 || d.smRel == isRel {
			d.smRel = isRel
			for _, s := range d.small[:d.nSmall] {
				if s == id {
					return false
				}
			}
			if int(d.nSmall) < len(d.small) {
				d.small[d.nSmall] = id
				d.nSmall++
				return true
			}
		}
		if d.smRel == isRel {
			for _, s := range d.small[:d.nSmall] {
				m.Add(s)
			}
		}
	}
	return m.Add(id)
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
	index  flatset.ByteMap
	indexI flatset.U64Map
	// keySlots holds each group key's row slot when EVERY key is a bare
	// variable reference (and the tuple is short enough to pack), else
	// nil. A bare variable's evaluation is exactly the row slot's value,
	// so the update hit path packs entity ids straight off the row and
	// probes -- skipping the per-row key evaluation and buffering that a
	// group HIT (the dominant case) would only throw away. Any row whose
	// slots don't pack, and every miss, falls through to the unchanged
	// generic path, so claim/seed logic exists once.
	keySlots []int
	// argSlots holds each aggregate argument's row slot when that argument
	// is a bare variable reference (-1 otherwise): the per-row argument
	// "evaluation" is then a direct slot read instead of a compiled-eval
	// dispatch. Same identity argument as keySlots.
	argSlots []int

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
	// pctC holds each percentile aggregate's compiled constant second
	// argument (nil for every other kind), evaluated once at finalize.
	pctC []RowEval

	// keyScratch/gkScratch/dkScratch are the per-update key buffers, reused
	// so routing a row to an existing group (or a seen DISTINCT value)
	// allocates nothing.
	keyScratch []value.Value
	gkScratch  []byte
	dkScratch  []byte
	// rec recycles DISTINCT-set slot arrays across this aggregation's
	// groups: thousands of per-group sets climb the same growth ladder,
	// and without pooling every doubling's outgrown array is garbage --
	// the dominant allocation of entity-DISTINCT aggregations over large
	// groups.
	rec flatset.Recycle
}

func newAggregator(ctx *eval.Ctx, proj *plan.ProjPlan, slots map[string]int) *aggregator {
	a := &aggregator{}
	for _, gi := range proj.GroupIdx {
		a.groupC = append(a.groupC, compileEval(ctx, proj.Returns[gi].Expr, slots))
	}
	if n := len(proj.GroupIdx); n == 1 || n == 2 {
		a.keySlots = make([]int, 0, n)
		for _, gi := range proj.GroupIdx {
			v, ok := proj.Returns[gi].Expr.(*ast.Var)
			if !ok {
				a.keySlots = nil
				break
			}
			s, ok := slots[v.Name]
			if !ok {
				a.keySlots = nil
				break
			}
			a.keySlots = append(a.keySlots, s)
		}
	}
	for _, ac := range proj.Aggs {
		argSlot := -1
		if ac.Arg != nil {
			a.aggC = append(a.aggC, compileEval(ctx, ac.Arg, slots))
			if v, ok := ac.Arg.(*ast.Var); ok {
				if s, ok2 := slots[v.Name]; ok2 {
					argSlot = s
				}
			}
		} else {
			a.aggC = append(a.aggC, nil)
		}
		a.argSlots = append(a.argSlots, argSlot)
		a.kinds = append(a.kinds, ac.Kind)
		if ac.Distinct {
			a.hasDistinct = true
		}
		switch ac.Kind {
		case plan.AggMin, plan.AggMax:
			a.hasMinMax = true
		case plan.AggCollect, plan.AggPercentileCont, plan.AggPercentileDisc:
			a.hasCollect = true
		}
		if ac.Arg2 != nil {
			a.pctC = append(a.pctC, compileEval(ctx, ac.Arg2, slots))
		} else {
			a.pctC = append(a.pctC, nil)
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
			seen := make([]distinctSet, chunkGroups*len(a.aggC))
			for i := range seen {
				seen[i].nodes.Rec = &a.rec
				seen[i].rels.Rec = &a.rec
			}
			a.seenChunks = append(a.seenChunks, seen)
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
		return packGroupKey1(keys[0])
	case 2:
		return packGroupKey2(keys[0], keys[1])
	}
	return 0, false
}

// packGroupKey1 is packGroupKey's single-key form.
func packGroupKey1(v value.Value) (uint64, bool) {
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
	return 0, false
}

// packGroupKey2 is packGroupKey's entity-pair form.
func packGroupKey2(a, b value.Value) (uint64, bool) {
	e1, ok1 := packedEntity30(a)
	e2, ok2 := packedEntity30(b)
	if ok1 && ok2 {
		return 3<<62 | e1<<31 | e2, true
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
		return a.indexI.GetOrCreate(gk64, func() int { return a.appendGroup(keys) })
	}
	gk := a.gkScratch[:0]
	for _, v := range keys {
		gk = value.AppendKey(gk, v)
	}
	a.gkScratch = gk
	return a.index.GetOrCreate(gk, func() int { return a.appendGroup(keys) })
}
