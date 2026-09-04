// The DISTINCT dedup machinery of the group-by aggregator: the per-group
// inline-then-probe-table set, and the banked recycler that carries the
// tables' backing arrays across groups and across aggregations. Split
// from aggregate.go, which holds the accumulator state and group-key
// packing.
package exec

import (
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/value"
)

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
			// The spill proves the group is past the inline size and still
			// growing: seed the probe table at 64 slots (48 adds before the
			// first grow), skipping the 16- and 32-slot rungs every spilled
			// group otherwise climbs through.
			m.Presize(64)
			for _, s := range d.small[:d.nSmall] {
				m.Add(s)
			}
		}
	}
	return m.Add(id)
}

// aggRecBank carries distinct-set recyclers across aggregator lifetimes.
// A strong bounded store, not a sync.Pool: aggregation-heavy queries
// allocate enough per run that a GC lands between runs, and sync.Pool's
// two-GC lifetime then frees the arrays before the next run can reuse
// them (measured 2% hit rate; the full attempt is preserved on
// research/agg-rec-pool). Bounds: 8 recyclers, 16MB filed each.
var aggRecBank = flatset.NewRecycleBank(8, 16<<20)

// releaseDistinct harvests every distinct set's slot array into the
// recycler and banks it. Called at the group-emitting terminals, which
// never read the sets (accumulators already hold their counts); the
// aggregator must not route further rows after it.
func (a *aggregator) releaseDistinct() {
	if a.rec == nil {
		return
	}
	for _, seen := range a.seenChunks {
		for i := range seen {
			seen[i].nodes.Release()
			seen[i].rels.Release()
		}
	}
	aggRecBank.Checkin(a.rec)
	a.rec = nil
}
