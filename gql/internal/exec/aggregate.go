// The group-by aggregator: rows route to their group's accumulators
// (implicit group-by-the-non-aggregate-keys), then one output row per
// group finalizes with ordering/pagination. count/sum/avg/min/max/collect
// with per-aggregate DISTINCT; nested-aggregate scalar wrappers read
// hidden accumulator slots and truncate them after. Encounter order of
// groups is preserved (observable in unordered results).
package exec

import (
	"math"
	"sync"

	chickpeas "github.com/freeeve/gochickpeas"

	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// aggState is one aggregate accumulator, allocated once per group per
// aggregate. It carries ONLY the count and the kind (16 B): the
// sum/avg/stddev numeric accumulators (64 B) live on numChunks, gated by
// hasNumAcc, and min/max/collect on their own slabs -- so a count-only
// grouping (14 of 24 LDBC aggregate stages, incl. the two largest) pays
// 16 B/group instead of 72, saving ~68 MB on Q4's 1.07M-group stage.
type aggState struct {
	count int64
	kind  plan.AggKind
}

// numAcc is the sum/avg/stddev heavy accumulator, on numChunks only when
// a Sum/Avg/Stddev aggregate exists. Fields wide-to-narrow so the
// booleans pack into the tail word.
type numAcc struct {
	sumI    acc128
	sumF    float64
	avgSum  float64
	avgN    int64
	sdMean  float64
	sdM2    float64
	isFloat bool
	any     bool
}

// update folds one argument in (arg absent means count(*)); na is the
// group's numAcc (nil for a count-only aggregator, never touched by the
// Count arm).
func (s *aggState) update(na *numAcc, arg value.Value, present bool) {
	switch s.kind {
	case plan.AggCount:
		if !present || !arg.IsNull() {
			s.count++
		}
	case plan.AggSum:
		if i, ok := arg.AsInt(); ok {
			na.sumI.add(i)
			na.any = true
		} else if arg.Kind() == value.KindFloat {
			f, _ := arg.AsFloat()
			na.sumF += f
			na.isFloat = true
			na.any = true
		}
	case plan.AggAvg:
		if f, ok := arg.AsFloat(); ok {
			na.avgSum += f
			na.avgN++
		}
	case plan.AggStddevSamp, plan.AggStddevPop:
		// Welford: numerically stable single pass; non-numeric args skip,
		// like avg.
		if f, ok := arg.AsFloat(); ok {
			na.avgN++
			d := f - na.sdMean
			na.sdMean += d / float64(na.avgN)
			na.sdM2 += d * (f - na.sdMean)
		}
	}
	// AggMin/AggMax/AggCollect are folded on the aggregator's overflow slabs,
	// not here (their heavy state is off the per-group struct).
}

// finalize emits the accumulator's value; na is the group's numAcc (nil
// for count-only kinds, which never read it).
func (s *aggState) finalize(na *numAcc) value.Value {
	switch s.kind {
	case plan.AggCount:
		return value.Int(s.count)
	case plan.AggSum:
		switch {
		case !na.any:
			return value.Int(0)
		case na.isFloat:
			return value.Float(na.sumF + na.sumI.float64())
		}
		// A true total outside int64 range is Null, matching the engine's
		// overflow policy (no per-row error channel) and the core
		// aggregate's nil Sum.
		if v, ok := na.sumI.int64(); ok {
			return value.Int(v)
		}
		return value.Null()
	case plan.AggAvg:
		if na.avgN == 0 {
			return value.Null()
		}
		return value.Float(na.avgSum / float64(na.avgN))
	case plan.AggStddevSamp:
		if na.avgN < 2 {
			return value.Float(0) // Neo4j's stdev: 0 on empty/single
		}
		return value.Float(math.Sqrt(na.sdM2 / float64(na.avgN-1)))
	case plan.AggStddevPop:
		if na.avgN == 0 {
			return value.Float(0)
		}
		return value.Float(math.Sqrt(na.sdM2 / float64(na.avgN)))
	}
	// AggMin/AggMax/AggCollect finalize off the aggregator's overflow slabs.
	return value.Null()
}

// Group state lives in flat stride-indexed slabs on the aggregator (keys,
// accumulators, DISTINCT sets) rather than per-group heap objects -- the
// grouping itself stays keyed on the canonical value encoding, identical
// to the Rust GroupKey set; the packed-uint64 index below is a perf-only
// representation, like the distinctSet entity-id fast path.

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

	nGroups int
	// keysPacked marks the packed-key regime: group key tuples live only
	// in packedKeys (one uint64 per group) and keysChunks stays empty
	// until a demotion. Entered when the first group arrives through the
	// packed index; left (forever, via demoteKeys) on the first group
	// whose keys cannot pack.
	keysPacked  bool
	packedKeys  []uint64
	keysChunks  [][]value.Value
	stateChunks [][]aggState
	seenChunks  [][]distinctSet   // filled only when a DISTINCT aggregate exists
	numChunks   [][]numAcc        // sum/avg/stddev heavy state, filled only when such an aggregate exists
	mmChunks    [][]value.Value   // min/max extrema, filled only when a min/max aggregate exists
	itemsChunks [][][]value.Value // collect lists, filled only when a collect aggregate exists
	hasDistinct bool
	hasNumAcc   bool
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
	// groups. Checked out of aggRecBank on the first DISTINCT slab and
	// checked back in -- with every set's FINAL array harvested -- at the
	// terminals, so the arrays survive across aggregations too: a warm
	// run's spill tables come from the previous run instead of the heap.
	rec *flatset.Recycle
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
		case plan.AggSum, plan.AggAvg, plan.AggStddevSamp, plan.AggStddevPop:
			a.hasNumAcc = true
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

// tierGroups are the geometric sizes of the first slab chunks: most
// aggregates group into tens-to-hundreds of rows, and a full-size first
// slab dominated whole-query allocation on small aggregates (task 205
// round 5: Q8's 218-group aggregate paid 0.63 MB of slab capacity per
// run). A large aggregate pays at most these three extra chunk seams.
var tierGroups = [3]int{128, 512, 2048}

// tierBounds are tierGroups' cumulative ends: the group index where each
// later chunk starts.
var tierBounds = [3]int{128, 640, 2688}

// chunkCap is chunk c's size in groups.
func chunkCap(c int) int {
	if c < len(tierGroups) {
		return tierGroups[c]
	}
	return chunkGroups
}

// chunkWindow maps a group index to its slab chunk and in-chunk group
// offset: the first chunks grow geometrically per tierGroups, every
// later chunk holds chunkGroups.
func chunkWindow(idx int) (c, w int) {
	switch {
	case idx < tierBounds[0]:
		return 0, idx
	case idx < tierBounds[1]:
		return 1, idx - tierBounds[0]
	case idx < tierBounds[2]:
		return 2, idx - tierBounds[1]
	}
	r := idx - tierBounds[2]
	return 3 + r/chunkGroups, r % chunkGroups
}

// keysOf/statesOf/seenOf are a group's slab windows.
func (a *aggregator) keysOf(idx int) []value.Value {
	k := len(a.groupC)
	c, w := chunkWindow(idx)
	return a.keysChunks[c][w*k : (w+1)*k]
}

func (a *aggregator) statesOf(idx int) []aggState {
	s := len(a.aggC)
	c, w := chunkWindow(idx)
	return a.stateChunks[c][w*s : (w+1)*s]
}

func (a *aggregator) seenOf(idx int) []distinctSet {
	s := len(a.aggC)
	c, w := chunkWindow(idx)
	return a.seenChunks[c][w*s : (w+1)*s]
}

func (a *aggregator) numOf(idx int) []numAcc {
	s := len(a.aggC)
	c, w := chunkWindow(idx)
	return a.numChunks[c][w*s : (w+1)*s]
}

func (a *aggregator) mmOf(idx int) []value.Value {
	s := len(a.aggC)
	c, w := chunkWindow(idx)
	return a.mmChunks[c][w*s : (w+1)*s]
}

func (a *aggregator) itemsOf(idx int) [][]value.Value {
	s := len(a.aggC)
	c, w := chunkWindow(idx)
	return a.itemsChunks[c][w*s : (w+1)*s]
}

// appendGroup claims the next slab windows for a new group, copying its
// key tuple in. The packed form (appendGroupPacked) claims the same
// windows but stores only the 8-byte packed key.
func (a *aggregator) appendGroup(keys []value.Value) int {
	idx := a.appendGroupSlots(false)
	c, _ := chunkWindow(idx)
	a.keysChunks[c] = append(a.keysChunks[c], keys...)
	return idx
}

// appendGroupPacked claims a group whose key tuple lives ONLY as its
// packed uint64: two boxed key Values per group (40 B each) were ~35% of
// a large group table's bytes (the sibling engine sized the identical
// redundancy at 45% of theirs, their 387) and are fully reconstructible
// from the pack tags. Sound only while every group packs; the first
// generic-path group demotes the whole table (demoteKeys).
func (a *aggregator) appendGroupPacked(gk64 uint64) int {
	idx := a.appendGroupSlots(true)
	if a.packedKeys == nil {
		a.packedKeys = packedKeyBank.checkout()
	}
	a.packedKeys = append(a.packedKeys, gk64)
	aggPackedKeyGroups++
	return idx
}

// appendGroupSlots is the shared slab bookkeeping.
func (a *aggregator) appendGroupSlots(packed bool) int {
	idx := a.nGroups
	a.nGroups++
	c, w := chunkWindow(idx)
	if w == 0 {
		n := chunkCap(c)
		if !packed {
			a.keysChunks = append(a.keysChunks, make([]value.Value, 0, n*len(a.groupC)))
		} else {
			a.keysChunks = append(a.keysChunks, nil)
		}
		a.stateChunks = append(a.stateChunks, make([]aggState, 0, n*len(a.aggC)))
		if a.hasDistinct {
			if a.rec == nil {
				a.rec = aggRecBank.Checkout()
			}
			seen := make([]distinctSet, n*len(a.aggC))
			for i := range seen {
				seen[i].nodes.Rec = a.rec
				seen[i].rels.Rec = a.rec
			}
			a.seenChunks = append(a.seenChunks, seen)
		}
		if a.hasNumAcc {
			a.numChunks = append(a.numChunks, make([]numAcc, n*len(a.aggC)))
		}
		if a.hasMinMax {
			a.mmChunks = append(a.mmChunks, make([]value.Value, n*len(a.aggC)))
		}
		if a.hasCollect {
			a.itemsChunks = append(a.itemsChunks, make([][]value.Value, n*len(a.aggC)))
		}
	}
	for _, k := range a.kinds {
		a.stateChunks[c] = append(a.stateChunks[c], aggState{kind: k})
	}
	return idx
}

// demoteKeys materializes every packed group's key tuple into the
// keysChunks slabs (replaying appendGroup's chunk geometry exactly, so
// emission order and window math are unchanged) and switches the
// aggregator to the general form -- called when a row's keys fail to
// pack after packed groups exist.
func (a *aggregator) demoteKeys() {
	if !a.keysPacked {
		return
	}
	a.keysPacked = false
	nk := len(a.groupC)
	for idx, gk := range a.packedKeys {
		c, w := chunkWindow(idx)
		if w == 0 && a.keysChunks[c] == nil {
			a.keysChunks[c] = make([]value.Value, 0, chunkCap(c)*nk)
		}
		a.keysChunks[c] = appendUnpackedKey(a.keysChunks[c], gk, nk)
	}
	packedKeyBank.checkin(a.packedKeys)
	a.packedKeys = nil
	aggKeyDemotions++
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

// packedKeyBank carries packedKeys backing slices across aggregator
// lifetimes: the []uint64 grows one geometric ladder per aggregation
// (~log2(groups) allocations to 1M on Q4) and is discarded whole at
// finalize -- and on a demotion the whole ladder becomes garbage (the
// mixed-type-key case, Q12/Q5). A bounded STRONG store, not a
// sync.Pool: Q4 allocates ~208 MB/run so a GC lands between runs and
// the pool's two-GC lifetime would drop the slabs (the task-360
// finding). Bounds: 2 slabs of at most 2M keys (16 MiB) each -- a 32
// MiB standing ceiling, covering Q4-scale million-group tables with
// headroom while keeping the strong store off RSS at scale (task 213).
var packedKeyBank = newU64SlabBank(2, 2<<20)

type u64SlabBank struct {
	mu      sync.Mutex
	free    [][]uint64
	keep    int
	maxKeep int
}

func newU64SlabBank(keep, maxKeep int) *u64SlabBank {
	return &u64SlabBank{keep: keep, maxKeep: maxKeep}
}

func (b *u64SlabBank) checkout() []uint64 {
	b.mu.Lock()
	if n := len(b.free); n > 0 {
		s := b.free[n-1]
		b.free = b.free[:n-1]
		b.mu.Unlock()
		return s[:0]
	}
	b.mu.Unlock()
	return nil
}

func (b *u64SlabBank) checkin(s []uint64) {
	if s == nil || cap(s) > b.maxKeep {
		return
	}
	b.mu.Lock()
	if len(b.free) < b.keep {
		b.free = append(b.free, s)
	}
	b.mu.Unlock()
}

// releasePackedKeys banks the packed-key backing (retaining capacity)
// and detaches it -- called once every group has been emitted.
func (a *aggregator) releasePackedKeys() {
	if a.packedKeys != nil {
		packedKeyBank.checkin(a.packedKeys)
		a.packedKeys = nil
	}
}

// aggPackedKeyGroups / aggKeyDemotions / disablePackedKeys are the
// packed-key regime's engagement counters and differential pin
// (sequential-reader constraint as documented on the chunk counters).
var (
	aggPackedKeyGroups int
	aggKeyDemotions    int
	disablePackedKeys  bool
)

// AggPackedKeyGroups exposes the engagement counter to tests.
func AggPackedKeyGroups() int { return aggPackedKeyGroups }

// AggKeyDemotions exposes the demotion counter to tests.
func AggKeyDemotions() int { return aggKeyDemotions }

// SetDisablePackedKeys pins comparisons to the materialized-key form.
func SetDisablePackedKeys(v bool) { disablePackedKeys = v }

// appendUnpackedKey reconstructs a packed group key's Value tuple from
// its pack tags -- the exact inverse of packGroupKey1/packGroupKey2.
func appendUnpackedKey(dst []value.Value, gk uint64, nk int) []value.Value {
	if nk == 2 {
		// Tag 3: two 31-bit (kind bit + 30-bit id) entities.
		e1 := (gk >> 31) & (1<<31 - 1)
		e2 := gk & (1<<31 - 1)
		return append(dst, unpackEntity30(e1), unpackEntity30(e2))
	}
	switch gk >> 62 {
	case 0:
		// 62-bit two's-complement integer: sign-extend from bit 61.
		v := gk & (1<<62 - 1)
		if v&(1<<61) != 0 {
			v |= uint64(0xC000000000000000)
		}
		return append(dst, value.Int(int64(v)))
	case 1:
		return append(dst, value.Node(chickpeas.NodeID(uint32(gk))))
	default: // 2
		return append(dst, value.Rel(uint32(gk)))
	}
}

// unpackEntity30 is packedEntity30's inverse.
func unpackEntity30(e uint64) value.Value {
	id := uint32(e & (1<<30 - 1))
	if e&(1<<30) != 0 {
		return value.Rel(id)
	}
	return value.Node(chickpeas.NodeID(id))
}
