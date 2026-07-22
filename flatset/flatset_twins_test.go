package flatset

import (
	"strconv"
	"testing"
)

// Coverage for the integer-keyed U64Set, the value-carrying ByteMap, and
// the U32Set/U64Map methods the other tests do not exercise (Reset, Built,
// ForEach). The set/map growth paths are driven past several doublings so
// the rehash branches run.

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

// TestByteMapGetOrCreateAndGet checks that GetOrCreate mints exactly once
// per distinct key, Get reflects the mapping, and create is never called on
// a hit.
func TestByteMapGetOrCreateAndGet(t *testing.T) {
	var m ByteMap
	if _, ok := m.Get([]byte("x")); ok {
		t.Fatal("empty map reports a hit")
	}
	next := 0
	mint := func() int { next++; return next }
	if got := m.GetOrCreate([]byte("a"), mint); got != 1 {
		t.Fatalf("GetOrCreate(a) = %d, want 1", got)
	}
	if got := m.GetOrCreate([]byte("a"), func() int { t.Fatal("create called on hit"); return -1 }); got != 1 {
		t.Fatalf("repeat GetOrCreate(a) = %d, want 1", got)
	}
	if got := m.GetOrCreate([]byte("b"), mint); got != 2 {
		t.Fatalf("GetOrCreate(b) = %d, want 2", got)
	}
	if m.Len() != 2 {
		t.Fatalf("Len = %d, want 2", m.Len())
	}
	if v, ok := m.Get([]byte("a")); !ok || v != 1 {
		t.Fatalf("Get(a) = %d,%v", v, ok)
	}
	if v, ok := m.Get([]byte("b")); !ok || v != 2 {
		t.Fatalf("Get(b) = %d,%v", v, ok)
	}
	if _, ok := m.Get([]byte("c")); ok {
		t.Fatal("Get(c) reported a hit for an absent key")
	}
}

// TestByteMapGrowAndKeyReuse drives ByteMap past several doublings and, at
// the same time, mutates the single scratch buffer between inserts to prove
// keys are copied (never aliased), as the doc guarantees.
func TestByteMapGrowAndKeyReuse(t *testing.T) {
	var m ByteMap
	const n = 400
	scratch := make([]byte, 0, 16)
	for i := range n {
		scratch = append(scratch[:0], strconv.Itoa(i)...)
		got := m.GetOrCreate(scratch, func() int { return i })
		if got != i {
			t.Fatalf("GetOrCreate(%d) = %d, want %d", i, got, i)
		}
	}
	if m.Len() != n {
		t.Fatalf("Len = %d, want %d", m.Len(), n)
	}
	for i := range n {
		scratch = append(scratch[:0], strconv.Itoa(i)...)
		v, ok := m.Get(scratch)
		if !ok || v != i {
			t.Fatalf("Get(%d) = %d,%v across grow", i, v, ok)
		}
	}
	if _, ok := m.Get([]byte("nope")); ok {
		t.Fatal("absent key reported present after grow")
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
