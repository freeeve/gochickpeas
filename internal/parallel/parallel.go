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

// chunks splits [0, n) into at most workers*4 contiguous chunks. Bounding the
// chunk count keeps per-chunk accumulator allocations small (a map-typed
// accumulator otherwise pays one allocation per chunk -- the pathology the
// Rust NodeSet::par_fold comment documents), while the x4 headroom keeps
// load balancing when chunks are uneven.
func chunks(n int) [][2]int {
	if n <= 0 {
		return nil
	}
	target := max(Workers(), 1) * 4
	size := max(n/target, 1)
	out := make([][2]int, 0, n/size+1)
	for lo := 0; lo < n; lo += size {
		out = append(out, [2]int{lo, min(lo+size, n)})
	}
	return out
}

// For runs body over [0, n) split into contiguous chunks on parallel
// goroutines and blocks until all complete.
func For(n int, body func(lo, hi int)) {
	cs := chunks(n)
	if len(cs) <= 1 {
		if n > 0 {
			body(0, n)
		}
		return
	}
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body(c[0], c[1])
		}()
	}
	wg.Wait()
}

// ForWorker is For with a worker identity: body additionally receives a
// stable index in [0, len(chunks)) so kernels can keep per-worker scratch
// (the Go stand-in for thread-locals) in a pre-sized slice. Distinct calls
// with the same index never run concurrently.
func ForWorker(n int, body func(worker, lo, hi int)) {
	cs := chunks(n)
	if len(cs) <= 1 {
		if n > 0 {
			body(0, 0, n)
		}
		return
	}
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body(i, c[0], c[1])
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
	cs := chunks(n)
	if len(cs) == 0 {
		return identity()
	}
	accs := make([]T, len(cs))
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
