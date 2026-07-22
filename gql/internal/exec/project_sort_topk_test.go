package exec

import (
	"slices"
	"testing"
)

// TestTopKIdx checks the top-K heap selection against a brute-force
// sort-then-truncate oracle across every k, including ties: topKIdx must
// return exactly the k smallest elements (as a set) under the comparator.
// It also exercises siftDownIdx, which topKIdx drives.
func TestTopKIdx(t *testing.T) {
	check := func(name string, vals []int) {
		t.Helper()
		cmp := func(a, b int) int { return vals[a] - vals[b] }
		freshIdx := func() []int {
			s := make([]int, len(vals))
			for i := range s {
				s[i] = i
			}
			return s
		}
		// oracle: the k smallest values, sorted.
		oracle := func(k int) []int {
			s := append([]int(nil), vals...)
			slices.Sort(s)
			return s[:k]
		}
		// the selected indices' values, sorted (order among them is
		// unspecified, so compare as a sorted multiset).
		valuesOf := func(idx []int) []int {
			out := make([]int, len(idx))
			for i, id := range idx {
				out[i] = vals[id]
			}
			slices.Sort(out)
			return out
		}
		for k := 0; k <= len(vals); k++ {
			got := topKIdx(freshIdx(), k, cmp)
			if len(got) != k {
				t.Fatalf("%s k=%d: len = %d", name, k, len(got))
			}
			if !slices.Equal(valuesOf(got), oracle(k)) {
				t.Fatalf("%s k=%d: got %v, want %v", name, k, valuesOf(got), oracle(k))
			}
		}
	}

	// Distinct values, large enough that the heap has multi-level sifts with
	// both children.
	check("distinct", []int{5, 3, 8, 1, 9, 2, 7, 4, 6, 0})
	// Ties must not break the set (total order, equal elements interchangeable).
	check("ties", []int{3, 1, 3, 1, 2, 1, 3})
	// Already sorted and reverse-sorted edge shapes.
	check("ascending", []int{0, 1, 2, 3, 4, 5})
	check("descending", []int{5, 4, 3, 2, 1, 0})
}
