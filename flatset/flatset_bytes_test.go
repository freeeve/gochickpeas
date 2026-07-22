package flatset

import (
	"strconv"
	"testing"
)

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
