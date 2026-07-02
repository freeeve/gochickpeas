package parallel_test

import (
	"sync/atomic"
	"testing"

	"github.com/freeeve/gochickpeas/internal/parallel"
)

func TestForCoversEveryIndexOnce(t *testing.T) {
	for _, n := range []int{0, 1, 7, 100, 100_000} {
		seen := make([]int32, n)
		parallel.For(n, func(lo, hi int) {
			for i := lo; i < hi; i++ {
				atomic.AddInt32(&seen[i], 1)
			}
		})
		for i, c := range seen {
			if c != 1 {
				t.Fatalf("n=%d: index %d visited %d times", n, i, c)
			}
		}
	}
}

func TestForWorkerScratchIsUncontended(t *testing.T) {
	// Worker indexes must be usable as scratch slots: distinct chunks with
	// the same worker index never run concurrently (each index is used once).
	n := 100_000
	var slots [4096]int64
	parallel.ForWorker(n, func(worker, lo, hi int) {
		slots[worker] += int64(hi - lo)
	})
	var total int64
	for _, s := range slots {
		total += s
	}
	if total != int64(n) {
		t.Fatalf("scratch total %d, want %d", total, n)
	}
}

func TestFoldMatchesSequential(t *testing.T) {
	for _, n := range []int{0, 1, 63, 1_000, 250_000} {
		got := parallel.Fold(n,
			func() int64 { return 0 },
			func(acc int64, i int) int64 { return acc + int64(i) },
			func(a, b int64) int64 { return a + b })
		want := int64(n) * int64(n-1) / 2
		if n == 0 {
			want = 0
		}
		if got != want {
			t.Fatalf("n=%d: got %d, want %d", n, got, want)
		}
	}
}

func TestFoldReducesInChunkOrder(t *testing.T) {
	// With an order-sensitive (but associative) reduce -- concatenation --
	// the result must be the in-order sequence, proving deterministic
	// chunk-order reduction.
	n := 10_000
	got := parallel.Fold(n,
		func() []int { return nil },
		func(acc []int, i int) []int { return append(acc, i) },
		func(a, b []int) []int { return append(a, b...) })
	for i, v := range got {
		if v != i {
			t.Fatalf("position %d holds %d", i, v)
		}
	}
}

func TestJoin(t *testing.T) {
	var a, b, c atomic.Int32
	parallel.Join(
		func() { a.Store(1) },
		func() { b.Store(2) },
		func() { c.Store(3) },
	)
	if a.Load() != 1 || b.Load() != 2 || c.Load() != 3 {
		t.Fatal("join did not run every closure")
	}
	parallel.Join() // no-op
	parallel.Join(func() { a.Store(9) })
	if a.Load() != 9 {
		t.Fatal("single-closure join did not run")
	}
}
