// Packed-key group storage: the reconstructible-uint64 key regime's
// backing bank, engagement counters, and the pack inverses. The
// aggregator stores a packable group's key tuple as its index uint64
// alone (appendGroupPacked/demoteKeys in aggregate.go) and rebuilds the
// boxed Values here at emission. Split from aggregate.go.
package exec

import (
	"sync"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

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
