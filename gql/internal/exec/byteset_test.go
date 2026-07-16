package exec

import (
	"fmt"
	"testing"
)

// TestByteSetDedup checks add reports first-sight vs duplicate correctly and
// that the caller may reuse (mutate) the scratch buffer between adds -- the
// set must have copied the bytes, not aliased them.
func TestByteSetDedup(t *testing.T) {
	var s byteSet
	scratch := make([]byte, 0, 8)
	seen := map[string]bool{}
	// Many distinct keys, interleaved with repeats, all through one reused
	// scratch buffer to exercise the arena copy.
	for i := 0; i < 5000; i++ {
		for _, k := range []int{i, i / 2, i / 3} {
			scratch = append(scratch[:0], fmt.Sprintf("key-%d", k)...)
			key := fmt.Sprintf("key-%d", k)
			got := s.add(scratch)
			want := !seen[key]
			if got != want {
				t.Fatalf("add(%q) = %v, want %v", key, got, want)
			}
			seen[key] = true
		}
	}
	if s.count != len(seen) {
		t.Fatalf("count = %d, want %d", s.count, len(seen))
	}
}

// TestByteSetArenaRealloc forces many arena growths and confirms earlier keys
// stay findable after the arena backing has reallocated several times.
func TestByteSetArenaRealloc(t *testing.T) {
	var s byteSet
	keys := make([][]byte, 2000)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%d-payload-payload-payload", i))
		if !s.add(keys[i]) {
			t.Fatalf("first add of key %d reported duplicate", i)
		}
	}
	for i, k := range keys {
		if s.add(k) {
			t.Fatalf("re-add of key %d reported new after arena growth", i)
		}
	}
}

// TestByteSetInlineAndSpill exercises the inline small-key path and the
// spill into the table form: dedup must hold across the boundary, for both
// short keys (inline-eligible) and a long key that forces the spill early.
func TestByteSetInlineAndSpill(t *testing.T) {
	var s byteSet
	short := [][]byte{[]byte("a1"), []byte("b22"), []byte("c333"), []byte("d4444")}
	for _, k := range short {
		if !s.add(k) {
			t.Fatalf("first add of %q reported duplicate", k)
		}
		if s.add(k) {
			t.Fatalf("re-add of %q (inline phase) reported new", k)
		}
	}
	// Fifth key spills; the inline keys must remain deduped afterward.
	if !s.add([]byte("e55555")) {
		t.Fatal("spilling key reported duplicate")
	}
	for _, k := range short {
		if s.add(k) {
			t.Fatalf("re-add of %q after spill reported new", k)
		}
	}
	if s.count != 5 {
		t.Fatalf("count = %d, want 5", s.count)
	}

	// A key too long for an inline slot forces the spill immediately.
	var s2 byteSet
	long := []byte("this key is far longer than an inline slot holds")
	if !s2.add([]byte("x")) || !s2.add(long) {
		t.Fatal("adds reported duplicates")
	}
	if s2.add(long) || s2.add([]byte("x")) {
		t.Fatal("post-spill re-adds reported new")
	}
	if s2.count != 2 {
		t.Fatalf("count = %d, want 2", s2.count)
	}
}

// TestByteSetEmptyAndSingle covers the empty set and single-element edge.
func TestByteSetEmptyAndSingle(t *testing.T) {
	var s byteSet
	if !s.add([]byte("x")) {
		t.Fatal("first add reported duplicate")
	}
	if s.add([]byte("x")) {
		t.Fatal("second add reported new")
	}
	if s.count != 1 {
		t.Fatalf("count = %d, want 1", s.count)
	}
}
