// Granularity A/B for the chunked-queue For against a plain one-range-
// per-worker split (task 198, the rustychickpeas f9b0cdb counter-finding):
// on UNIFORM per-item work the balance machinery is claimed pure overhead,
// while clustered skew is what the queue exists for. The workloads bracket
// the engine's real callers -- column-scan-uniform on one side, PageRank/
// CDLP neighbor walks (degree skew) on the other.
package parallel

import (
	"sync"
	"testing"
)

// rangePerWorker is the no-queue shape under test: [0, n) split into one
// contiguous range per worker, no oversplit, no shared counter.
func rangePerWorker(n int, body func(lo, hi int)) {
	workers := min(n, max(Workers(), 1))
	if workers <= 1 {
		if n > 0 {
			body(0, n)
		}
		return
	}
	size := (n + workers - 1) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		lo, hi := w*size, min((w+1)*size, n)
		go func() {
			defer wg.Done()
			body(lo, hi)
		}()
	}
	wg.Wait()
}

const benchN = 1 << 21 // ~2M, matching the Rust measurement's scale

var sink float64

// uniformBody is the uniform-cost workload: a few flops per index into a
// caller-provided partial, no allocation, no branch skew.
func uniformBody(dst []float64) func(lo, hi int) {
	return func(lo, hi int) {
		s := 0.0
		for i := lo; i < hi; i++ {
			x := float64(i)
			s += x * 1.0000001
		}
		dst[lo%len(dst)] += s
	}
}

// clusteredBody front-loads the cost: the first 1/16 of the index space
// does 64x the work (a contiguous hot region, the shape a static split
// cannot balance).
func clusteredBody(dst []float64) func(lo, hi int) {
	hot := benchN / 16
	return func(lo, hi int) {
		s := 0.0
		for i := lo; i < hi; i++ {
			reps := 1
			if i < hot {
				reps = 64
			}
			x := float64(i)
			for range reps {
				s += x * 1.0000001
			}
		}
		dst[lo%len(dst)] += s
	}
}

// scatteredBody spreads the same total skew evenly: every 16th index does
// 64x the work, so any contiguous split is already balanced.
func scatteredBody(dst []float64) func(lo, hi int) {
	return func(lo, hi int) {
		s := 0.0
		for i := lo; i < hi; i++ {
			reps := 1
			if i%16 == 0 {
				reps = 64
			}
			x := float64(i)
			for range reps {
				s += x * 1.0000001
			}
		}
		dst[lo%len(dst)] += s
	}
}

func benchBoth(b *testing.B, mk func([]float64) func(lo, hi int)) {
	dst := make([]float64, 512)
	b.Run("queue", func(b *testing.B) {
		b.ReportAllocs()
		body := mk(dst)
		for b.Loop() {
			For(benchN, body)
		}
		sink += dst[0]
	})
	b.Run("range-per-worker", func(b *testing.B) {
		b.ReportAllocs()
		body := mk(dst)
		for b.Loop() {
			rangePerWorker(benchN, body)
		}
		sink += dst[0]
	})
}

func BenchmarkForUniform(b *testing.B)       { benchBoth(b, uniformBody) }
func BenchmarkForClusteredSkew(b *testing.B) { benchBoth(b, clusteredBody) }
func BenchmarkForScatteredSkew(b *testing.B) { benchBoth(b, scatteredBody) }
