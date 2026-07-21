// FoldInto/Fold contract stress (task 203): caller-owned accumulators
// with reduce functions that alias accs[0], reuse across calls with the
// documented reset, and order-sensitive reduces at worker-count
// boundaries -- against single-threaded references.
package parallel

import (
	"math/rand"
	"slices"
	"testing"
)

// TestFoldIntoAliasingReduce runs a reduce that appends INTO its left
// argument (aliasing accs[0], the documented return-aliases-accs[0]
// shape) across reuse cycles, for n at and around the worker count.
func TestFoldIntoAliasingReduce(t *testing.T) {
	w := Workers()
	accs := make([][]int, w)
	for _, n := range []int{0, 1, w - 1, w, w + 1, 3 * w, 1000} {
		if n < 0 {
			continue
		}
		// Reset contract: every accumulator emptied before the call.
		for i := range accs {
			accs[i] = accs[i][:0]
		}
		got := FoldInto(accs, n,
			func(acc []int, i int) []int { return append(acc, i*i) },
			func(a, b []int) []int { return append(a, b...) })
		want := make([]int, 0, n)
		for i := 0; i < n; i++ {
			want = append(want, i*i)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("n=%d: got %d elems, in-order contract broken", n, len(got))
		}
	}
}

// TestFoldIntoMapAccsReuse reuses map accumulators across many calls
// with clear() (the warm-call zero-alloc pattern): counts must match a
// reference every cycle -- stale entries surviving a reset would drift.
func TestFoldIntoMapAccsReuse(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	w := Workers()
	accs := make([]map[int]int, w)
	for i := range accs {
		accs[i] = map[int]int{}
	}
	for cycle := 0; cycle < 30; cycle++ {
		n := rng.Intn(5000)
		keys := make([]int, n)
		for i := range keys {
			keys[i] = rng.Intn(97)
		}
		for i := range accs {
			clear(accs[i])
		}
		got := FoldInto(accs, n,
			func(acc map[int]int, i int) map[int]int { acc[keys[i]]++; return acc },
			func(a, b map[int]int) map[int]int {
				for k, v := range b {
					a[k] += v
				}
				return a
			})
		ref := map[int]int{}
		for _, k := range keys {
			ref[k]++
		}
		if len(got) != len(ref) {
			t.Fatalf("cycle %d: %d keys, want %d", cycle, len(got), len(ref))
		}
		for k, v := range ref {
			if got[k] != v {
				t.Fatalf("cycle %d: key %d = %d, want %d", cycle, k, got[k], v)
			}
		}
	}
}
