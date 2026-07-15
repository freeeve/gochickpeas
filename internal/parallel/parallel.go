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

// Fold is the rayon fold/reduce shape over [0, n): one accumulator per chunk
// (never per element), seeded by identity, folded over the chunk's indices,
// then merged with reduce in ascending chunk order -- so the result is
// deterministic for any associative reduce. identity is called once per
// chunk plus once for an empty input.
func Fold[T any](n int, identity func() T, fold func(acc T, i int) T, reduce func(a, b T) T) T {
	count, _ := chunkPlan(n)
	if count == 0 {
		return identity()
	}
	accs := make([]T, count)
	ForWorker(n, func(worker, lo, hi int) {
		acc := identity()
		for i := lo; i < hi; i++ {
			acc = fold(acc, i)
		}
		accs[worker] = acc
	})
	out := accs[0]
	for _, acc := range accs[1:] {
		out = reduce(out, acc)
	}
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
