package flatset

import (
	"fmt"
	"testing"
)

// TestByteSetDedup checks add reports first-sight vs duplicate correctly and
// that the caller may reuse (mutate) the scratch buffer between adds -- the
// set must have copied the bytes, not aliased them.
func TestByteSetDedup(t *testing.T) {
	var s ByteSet
	scratch := make([]byte, 0, 8)
	seen := map[string]bool{}
	// Many distinct keys, interleaved with repeats, all through one reused
	// scratch buffer to exercise the arena copy.
	for i := 0; i < 5000; i++ {
		for _, k := range []int{i, i / 2, i / 3} {
			scratch = append(scratch[:0], fmt.Sprintf("key-%d", k)...)
			key := fmt.Sprintf("key-%d", k)
			got := s.Add(scratch)
			want := !seen[key]
			if got != want {
				t.Fatalf("add(%q) = %v, want %v", key, got, want)
			}
			seen[key] = true
		}
	}
	if s.Len() != len(seen) {
		t.Fatalf("count = %d, want %d", s.Len(), len(seen))
	}
}

// TestByteSetArenaRealloc forces many arena growths and confirms earlier keys
// stay findable after the arena backing has reallocated several times.
func TestByteSetArenaRealloc(t *testing.T) {
	var s ByteSet
	keys := make([][]byte, 2000)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%d-payload-payload-payload", i))
		if !s.Add(keys[i]) {
			t.Fatalf("first add of key %d reported duplicate", i)
		}
	}
	for i, k := range keys {
		if s.Add(k) {
			t.Fatalf("re-add of key %d reported new after arena growth", i)
		}
	}
}

// TestByteSetInlineAndSpill exercises the inline small-key path and the
// spill into the table form: dedup must hold across the boundary, for both
// short keys (inline-eligible) and a long key that forces the spill early.
func TestByteSetInlineAndSpill(t *testing.T) {
	var s ByteSet
	short := [][]byte{[]byte("a1"), []byte("b22"), []byte("c333"), []byte("d4444")}
	for _, k := range short {
		if !s.Add(k) {
			t.Fatalf("first add of %q reported duplicate", k)
		}
		if s.Add(k) {
			t.Fatalf("re-add of %q (inline phase) reported new", k)
		}
	}
	// Fifth key spills; the inline keys must remain deduped afterward.
	if !s.Add([]byte("e55555")) {
		t.Fatal("spilling key reported duplicate")
	}
	for _, k := range short {
		if s.Add(k) {
			t.Fatalf("re-add of %q after spill reported new", k)
		}
	}
	if s.Len() != 5 {
		t.Fatalf("count = %d, want 5", s.Len())
	}

	// A key too long for an inline slot forces the spill immediately.
	var s2 ByteSet
	long := []byte("this key is far longer than an inline slot holds")
	if !s2.Add([]byte("x")) || !s2.Add(long) {
		t.Fatal("adds reported duplicates")
	}
	if s2.Add(long) || s2.Add([]byte("x")) {
		t.Fatal("post-spill re-adds reported new")
	}
	if s2.count != 2 {
		t.Fatalf("count = %d, want 2", s2.count)
	}
}

// TestByteSetEmptyAndSingle covers the empty set and single-element edge.
func TestByteSetEmptyAndSingle(t *testing.T) {
	var s ByteSet
	if !s.Add([]byte("x")) {
		t.Fatal("first add reported duplicate")
	}
	if s.Add([]byte("x")) {
		t.Fatal("second add reported new")
	}
	if s.Len() != 1 {
		t.Fatalf("count = %d, want 1", s.Len())
	}
}

// TestU32SetRecycle pins the shared-recycler contract: many sets growing
// through the same ladder stay membership-exact while outgrown arrays are
// reused across sets (a reused array must come back zeroed -- a stale
// slot would fabricate members).
func TestU32SetRecycle(t *testing.T) {
	var rec Recycle
	const sets, n = 40, 500
	ss := make([]U32Set, sets)
	for i := range ss {
		ss[i].Rec = &rec
		for v := 0; v < n; v++ {
			id := uint32(i*100000 + v*7)
			if !ss[i].Add(id) {
				t.Fatalf("set %d: fresh id %d reported seen", i, id)
			}
			if ss[i].Add(id) {
				t.Fatalf("set %d: duplicate id %d reported new", i, id)
			}
		}
	}
	for i := range ss {
		if ss[i].Len() != n {
			t.Fatalf("set %d: len %d, want %d", i, ss[i].Len(), n)
		}
		for v := 0; v < n; v++ {
			if !ss[i].Has(uint32(i*100000 + v*7)) {
				t.Fatalf("set %d: lost id %d", i, v*7)
			}
		}
		if ss[i].Has(uint32(i*100000 + 3)) {
			t.Fatal("phantom member from a recycled array")
		}
	}
}

// TestU64SetAddHasLenReset walks the basic membership contract: a first Add
// is newly-seen, a repeat is not, Has tracks membership, and Reset empties
// the set while keeping it usable.
func TestU64SetAddHasLenReset(t *testing.T) {
	var s U64Set
	if s.Has(42) {
		t.Fatal("empty set reports membership")
	}
	if s.Len() != 0 {
		t.Fatalf("empty Len = %d", s.Len())
	}
	if !s.Add(42) {
		t.Fatal("first Add(42) should be newly seen")
	}
	if s.Add(42) {
		t.Fatal("second Add(42) should report already present")
	}
	if !s.Add(7) || !s.Add(1<<40) {
		t.Fatal("distinct Adds should be newly seen")
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	if !s.Has(42) || !s.Has(7) || !s.Has(1<<40) {
		t.Fatal("added keys must be present")
	}
	if s.Has(43) {
		t.Fatal("absent key reported present")
	}
	s.Reset()
	if s.Len() != 0 || s.Has(42) {
		t.Fatalf("after Reset: Len %d, Has(42) %v", s.Len(), s.Has(42))
	}
	if !s.Add(42) {
		t.Fatal("Add after Reset should be newly seen again")
	}
}

// TestU64SetGrow forces several table doublings and checks every key
// survives the rehash while a known-absent key stays absent.
func TestU64SetGrow(t *testing.T) {
	var s U64Set
	const n = 500
	for i := range n {
		key := uint64(i)<<32 | uint64(i*7+1)
		if !s.Add(key) {
			t.Fatalf("Add of distinct key %d reported duplicate", i)
		}
	}
	if s.Len() != n {
		t.Fatalf("Len = %d, want %d", s.Len(), n)
	}
	for i := range n {
		key := uint64(i)<<32 | uint64(i*7+1)
		if !s.Has(key) {
			t.Fatalf("key %d lost across grow", i)
		}
	}
	if s.Has(uint64(n)<<32 | 999999) {
		t.Fatal("never-added key reported present after grow")
	}
	// Re-adding an existing key never grows or recounts.
	if s.Add(uint64(0)<<32 | 1) {
		t.Fatal("re-Add of an existing key reported newly seen")
	}
}

// TestU32SetResetAndBuilt covers Built (materialization flag) and Reset,
// which the recycle test does not touch directly.
func TestU32SetResetAndBuilt(t *testing.T) {
	var s U32Set
	if s.Built() {
		t.Fatal("fresh U32Set reports Built")
	}
	if s.Has(3) {
		t.Fatal("fresh U32Set reports membership")
	}
	s.Add(3)
	s.Add(9)
	if !s.Built() {
		t.Fatal("set with an Add should be Built")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	s.Reset()
	if s.Len() != 0 || s.Has(3) {
		t.Fatalf("after Reset: Len %d, Has(3) %v", s.Len(), s.Has(3))
	}
	if !s.Built() {
		t.Fatal("Reset should keep the slot backing (still Built)")
	}
	if !s.Add(3) {
		t.Fatal("Add after Reset should be newly seen again")
	}
}

// TestU64MapForEach checks ForEach visits every mapped pair exactly once.
func TestU64MapForEach(t *testing.T) {
	var m U64Map
	want := map[uint64]int{10: 100, 20: 200, 1 << 33: 300}
	for k, v := range want {
		val := v
		if got := m.GetOrCreate(k, func() int { return val }); got != v {
			t.Fatalf("GetOrCreate(%d) = %d, want %d", k, got, v)
		}
	}
	seen := map[uint64]int{}
	m.ForEach(func(key uint64, val int) { seen[key] = val })
	if len(seen) != len(want) {
		t.Fatalf("ForEach visited %d pairs, want %d", len(seen), len(want))
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("ForEach key %d = %d, want %d", k, seen[k], v)
		}
	}
	if _, ok := m.Get(999); ok {
		t.Fatal("Get of an absent key reported a hit")
	}
}
