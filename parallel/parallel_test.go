package parallel_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/freeeve/gochickpeas/parallel"
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

// TestChunksMatchesForWorkerIndexRange guards the Chunks contract kernels
// rely on to size persistent per-worker scratch (task 153): Chunks(n) must
// equal the exclusive upper bound of the worker index ForWorker hands out,
// so scratch[worker] is always in range. An off-by-one here would panic a
// pooling kernel (e.g. CDLP) with an index-out-of-range on the last chunk.
func TestForWorkerPoolContract(t *testing.T) {
	// Worker indexes are pool-worker ids bounded by min(Workers, Chunks),
	// and the chunks cover [0, n) exactly once.
	for _, n := range []int{0, 1, 2, 7, 63, 100, 13_003, 100_000, 832_247} {
		bound := parallel.Workers()
		if c := parallel.Chunks(n); c < bound {
			bound = c
		}
		covered := make([]int32, n)
		maxWorker := -1
		var mu sync.Mutex
		parallel.ForWorker(n, func(worker, lo, hi int) {
			mu.Lock()
			if worker > maxWorker {
				maxWorker = worker
			}
			mu.Unlock()
			for i := lo; i < hi; i++ {
				atomic.AddInt32(&covered[i], 1)
			}
		})
		if n == 0 {
			if maxWorker != -1 {
				t.Fatalf("n=0: body ran")
			}
			continue
		}
		if maxWorker >= bound {
			t.Fatalf("n=%d: worker index %d >= bound %d", n, maxWorker, bound)
		}
		for i, c := range covered {
			if c != 1 {
				t.Fatalf("n=%d: index %d covered %d times", n, i, c)
			}
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

func TestFoldIntoInOrderAndReusable(t *testing.T) {
	// Same in-order contract as Fold, driven twice through the SAME
	// accumulators to pin the reuse story (reset between calls).
	n := 10_000
	accs := make([][]int, parallel.Workers())
	for round := 0; round < 2; round++ {
		for i := range accs {
			accs[i] = accs[i][:0]
		}
		got := parallel.FoldInto(accs, n,
			func(acc []int, i int) []int { return append(acc, i) },
			func(a, b []int) []int { return append(a, b...) })
		for i, v := range got {
			if v != i {
				t.Fatalf("round %d: position %d holds %d", round, i, v)
			}
		}
	}
}

func TestFoldIntoWarmAllocs(t *testing.T) {
	// A warm call with map accumulators must not allocate for accumulator
	// state: buckets persist through clear().
	n := 50_000
	accs := make([]map[int]int64, parallel.Workers())
	for i := range accs {
		accs[i] = map[int]int64{}
	}
	run := func() {
		for _, m := range accs {
			clear(m)
		}
		parallel.FoldInto(accs, n,
			func(acc map[int]int64, i int) map[int]int64 { acc[i%512]++; return acc },
			func(a, b map[int]int64) map[int]int64 {
				for k, v := range b {
					a[k] += v
				}
				return a
			})
	}
	run() // reach high-water
	allocs := testing.AllocsPerRun(3, run)
	// Goroutine machinery only; the maps themselves must be silent.
	if allocs > 64 {
		t.Fatalf("warm FoldInto allocated %.0f objects (want goroutine machinery only)", allocs)
	}
}
