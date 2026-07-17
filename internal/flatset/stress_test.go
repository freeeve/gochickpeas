// Arena/recycler property stress (task 203): the flat structures against
// reference implementations under seeded-random adversarial shapes --
// interleaved growth across many recycler-sharing sets, duplicate-heavy
// streams, byte keys straddling the inline/arena spill, and empty/huge
// boundary sizes. Determinism via fixed seeds; failures print the seed.
package flatset

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestU32SetRecycleInterleavedProperty grows many recycler-sharing sets
// in randomized interleave against reference maps: membership answers
// and lengths must match exactly at every step -- a recycled array
// returned unzeroed, or handed to two sets, diverges immediately.
func TestU32SetRecycleInterleavedProperty(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var rec Recycle
		const sets = 24
		ss := make([]U32Set, sets)
		refs := make([]map[uint32]bool, sets)
		for i := range ss {
			ss[i].Rec = &rec
			refs[i] = map[uint32]bool{}
		}
		for step := 0; step < 200_000; step++ {
			i := rng.Intn(sets)
			// Skewed values: some sets grow big, some stay small, ids
			// collide across sets on purpose.
			id := uint32(rng.Intn(1 << uint(8+i%12)))
			fresh := ss[i].Add(id)
			if fresh == refs[i][id] {
				t.Fatalf("seed %d step %d: set %d Add(%d) fresh=%v, ref disagrees", seed, step, i, id, fresh)
			}
			refs[i][id] = true
			if rng.Intn(64) == 0 {
				probe := uint32(rng.Intn(1 << 16))
				if ss[i].Has(probe) != refs[i][probe] {
					t.Fatalf("seed %d step %d: set %d Has(%d) diverged", seed, step, i, probe)
				}
			}
		}
		for i := range ss {
			if ss[i].Len() != len(refs[i]) {
				t.Fatalf("seed %d: set %d len %d, ref %d", seed, i, ss[i].Len(), len(refs[i]))
			}
		}
	}
}

// TestByteSetSpillProperty hammers ByteSet across the inline/arena spill
// with duplicate-heavy variable-length keys, including empty and huge
// values, against a reference map.
func TestByteSetSpillProperty(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var s ByteSet
		ref := map[string]bool{}
		key := func() []byte {
			switch rng.Intn(10) {
			case 0:
				return nil // empty key
			case 1:
				b := make([]byte, 1+rng.Intn(4096)) // huge key
				rng.Read(b)
				return b
			default:
				// Small keys with heavy duplication: the inline claim and
				// its spill are the hazard zone.
				return fmt.Appendf(nil, "k%d", rng.Intn(300))
			}
		}
		for step := 0; step < 60_000; step++ {
			k := key()
			fresh := s.Add(k)
			if fresh == ref[string(k)] {
				t.Fatalf("seed %d step %d: Add(%q...) fresh=%v, ref disagrees", seed, step, k[:min(len(k), 12)], fresh)
			}
			ref[string(k)] = true
		}
		if s.Len() != len(ref) {
			t.Fatalf("seed %d: len %d, ref %d", seed, s.Len(), len(ref))
		}
	}
}

// TestU64MapResetReuse pins the Reset contract under reuse: values
// survive within a generation, vanish across Reset, and the buckets'
// reuse never resurrects stale entries.
func TestU64MapResetReuse(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var m U64Map
	for gen := 0; gen < 50; gen++ {
		ref := map[uint64]int{}
		next := 0
		for step := 0; step < 2000; step++ {
			k := uint64(rng.Intn(500))<<32 | uint64(rng.Intn(1000))
			got := m.GetOrCreate(k, func() int { v := next; next++; return v })
			want, ok := ref[k]
			if !ok {
				ref[k] = got
			} else if got != want {
				t.Fatalf("gen %d: GetOrCreate(%d) = %d, want %d", gen, k, got, want)
			}
		}
		if m.Len() != len(ref) {
			t.Fatalf("gen %d: len %d, ref %d", gen, m.Len(), len(ref))
		}
		for k, want := range ref {
			if got, ok := m.Get(k); !ok || got != want {
				t.Fatalf("gen %d: Get(%d) = %d,%v want %d", gen, k, got, ok, want)
			}
		}
		m.Reset()
		if m.Len() != 0 {
			t.Fatalf("gen %d: Reset left len %d", gen, m.Len())
		}
		if _, ok := m.Get(12345); ok {
			t.Fatalf("gen %d: stale entry survived Reset", gen)
		}
	}
}
