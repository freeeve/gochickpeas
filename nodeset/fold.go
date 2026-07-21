package nodeset

import (
	"github.com/freeeve/gochickpeas/parallel"
)

// ParFold folds the set's ids in parallel, then merges the per-chunk
// results: identity seeds each chunk's accumulator, fold folds one id into
// an accumulator, and reduce merges two accumulators (merged in ascending
// chunk order, so the result is deterministic for any associative reduce).
// The parallelism is an implementation detail -- the signature is pure std,
// so callers depend on neither a parallelism package nor the storage layout.
// Uses the contiguous-range fast path (AsRange) when the ids are gap-free,
// otherwise a collected scan. For a sequential fold use Iter.
//
// A package-level generic function rather than a method: Go methods cannot
// have type parameters.
func ParFold[T any](s *Set, identity func() T, fold func(acc T, id uint32) T, reduce func(a, b T) T) T {
	if lo, _, ok := s.AsRange(); ok {
		return parallel.Fold(s.Len(), identity,
			func(acc T, i int) T { return fold(acc, lo+uint32(i)) },
			reduce)
	}
	ids := s.ToSlice()
	return parallel.Fold(len(ids), identity,
		func(acc T, i int) T { return fold(acc, ids[i]) },
		reduce)
}
