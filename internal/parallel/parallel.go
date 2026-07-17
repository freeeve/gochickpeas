// Package parallel provides the chunked worker-pool primitives the engine's
// kernels build on -- the Go stand-in for rayon's fold/reduce shape. Public
// engine signatures never expose this package; parallelism stays an
// implementation detail behind pure-std func values.
package parallel

import (
	"runtime"
	"sync"
)

// Workers is the parallelism ceiling: GOMAXPROCS.
func Workers() int {
	return runtime.GOMAXPROCS(0)
}

// chunkPlan splits [0, n) into `count` contiguous chunks of `size` (the last
// short), count at most workers*4. Bounding the chunk count keeps per-chunk
// accumulator allocations small (a map-typed accumulator otherwise pays one
// allocation per chunk -- the pathology the Rust NodeSet::par_fold comment
// documents), while the x4 headroom keeps load balancing when chunks are
// uneven. Chunk k spans [k*size, min((k+1)*size, n)); returning the plan as
// two ints rather than a materialised []​[2]int keeps For/ForWorker/Fold from
// allocating a slice on every call. count is 0 for n <= 0.
func chunkPlan(n int) (count, size int) {
	if n <= 0 {
		return 0, 0
	}
	target := max(Workers(), 1) * 4
	size = max(n/target, 1)
	return (n + size - 1) / size, size
}

// Chunks reports how many contiguous chunks For/ForWorker/Fold split [0, n)
// into -- i.e. the exclusive upper bound of the worker index ForWorker hands
// out. Kernels that keep per-worker scratch persistent across repeated passes
// size their scratch slice by this so every worker index has a slot.
func Chunks(n int) int {
	count, _ := chunkPlan(n)
	return count
}

// For runs body over [0, n) split into contiguous chunks on parallel
// goroutines and blocks until all complete.
func For(n int, body func(lo, hi int)) {
	count, size := chunkPlan(n)
	if count <= 1 {
		if n > 0 {
			body(0, n)
		}
		return
	}
	var wg sync.WaitGroup
	wg.Add(count)
	for k := range count {
		lo, hi := k*size, min((k+1)*size, n)
		go func() {
			defer wg.Done()
			body(lo, hi)
		}()
	}
	wg.Wait()
}

// ForWorker is For with a worker identity: body additionally receives a
// stable index in [0, Chunks(n)) so kernels can keep per-worker scratch (the
// Go stand-in for thread-locals) in a pre-sized slice. Distinct chunks never
// share an index within a call, and a given index is used at most once per
// call, so scratch[worker] is safe to reuse across successive calls.
func ForWorker(n int, body func(worker, lo, hi int)) {
	count, size := chunkPlan(n)
	if count <= 1 {
		if n > 0 {
			body(0, 0, n)
		}
		return
	}
	var wg sync.WaitGroup
	wg.Add(count)
	for k := range count {
		worker, lo, hi := k, k*size, min((k+1)*size, n)
		go func() {
			defer wg.Done()
			body(worker, lo, hi)
		}()
	}
	wg.Wait()
}

// Fold is the rayon fold/reduce shape over [0, n): one accumulator per
// WORKER (never per chunk or element), each worker folding one contiguous
// index range in ascending order, then merged with reduce in ascending
// worker order -- so an order-sensitive associative reduce (a concat)
// still sees the fully in-order sequence, exactly like the former
// one-accumulator-per-chunk form. A heavy accumulator (a count slab, a
// map) is built once per worker instead of once per 4x-oversplit chunk;
// the oversplit's load-balancing headroom is deliberately traded away
// here because a fold's per-index cost is near-uniform in the kernels
// (For/ForWorker keep it). identity is called once per worker plus once
// for an empty input.
func Fold[T any](n int, identity func() T, fold func(acc T, i int) T, reduce func(a, b T) T) T {
	if n <= 0 {
		return identity()
	}
	workers := min(n, max(Workers(), 1))
	if workers <= 1 {
		acc := identity()
		for i := 0; i < n; i++ {
			acc = fold(acc, i)
		}
		return acc
	}
	size := (n + workers - 1) / workers
	accs := make([]T, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		lo, hi := w*size, min((w+1)*size, n)
		go func() {
			defer wg.Done()
			acc := identity()
			for i := lo; i < hi; i++ {
				acc = fold(acc, i)
			}
			accs[w] = acc
		}()
	}
	wg.Wait()
	out := accs[0]
	for _, acc := range accs[1:] {
		out = reduce(out, acc)
	}
	return out
}

// FoldInto is Fold with caller-owned worker accumulators: accs supplies
// one pre-seeded accumulator per worker (len(accs) caps the parallelism,
// further capped by n), each worker folds its contiguous in-order index
// range into its slot, and reduce merges in ascending worker order --
// the same in-order contract as Fold. The returned value aliases accs[0]
// for reference-typed T, and workers write accs[w] = fold(...), so the
// caller must reset EVERY accumulator before the next call. Reuse across
// calls is the point: a map keeps its buckets through clear(), a slab
// keeps its backing, so a warm call allocates nothing for accumulator
// state.
func FoldInto[T any](accs []T, n int, fold func(acc T, i int) T, reduce func(a, b T) T) T {
	if n <= 0 || len(accs) == 0 {
		var zero T
		if len(accs) > 0 {
			return accs[0]
		}
		return zero
	}
	workers := min(n, len(accs))
	if workers <= 1 {
		acc := accs[0]
		for i := 0; i < n; i++ {
			acc = fold(acc, i)
		}
		accs[0] = acc
		return acc
	}
	size := (n + workers - 1) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		lo, hi := w*size, min((w+1)*size, n)
		go func() {
			defer wg.Done()
			acc := accs[w]
			for i := lo; i < hi; i++ {
				acc = fold(acc, i)
			}
			accs[w] = acc
		}()
	}
	wg.Wait()
	out := accs[0]
	for _, acc := range accs[1:workers] {
		out = reduce(out, acc)
	}
	accs[0] = out
	return out
}

// Join runs the given closures concurrently and blocks until all complete --
// the finalize-style N-way join.
func Join(fns ...func()) {
	if len(fns) == 1 {
		fns[0]()
		return
	}
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	wg.Wait()
}
