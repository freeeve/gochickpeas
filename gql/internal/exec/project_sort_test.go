package exec

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
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

// TestPaginate covers OFFSET/SKIP-then-LIMIT windowing: skip and limit apply
// independently and compose, a skip at or past the end empties the result,
// and a limit past the end is a no-op.
func TestPaginate(t *testing.T) {
	p := func(n uint64) *uint64 { return &n }
	rows := [][]value.Value{urow(0), urow(1), urow(2), urow(3), urow(4)}

	// No bounds: unchanged.
	if got := paginate(rows, nil, nil); !rowsEqual(got, rows) {
		t.Fatalf("no bounds = %v", got)
	}
	// Skip drops the leading rows; a skip at/past the end empties.
	if got := paginate(rows, p(2), nil); !rowsEqual(got, rows[2:]) {
		t.Fatalf("skip 2 = %v", got)
	}
	if got := paginate(rows, p(5), nil); got != nil {
		t.Fatalf("skip == len = %v, want nil", got)
	}
	if got := paginate(rows, p(99), nil); got != nil {
		t.Fatalf("skip past end = %v, want nil", got)
	}
	// Limit truncates; a limit past the end is a no-op.
	if got := paginate(rows, nil, p(3)); !rowsEqual(got, rows[:3]) {
		t.Fatalf("limit 3 = %v", got)
	}
	if got := paginate(rows, nil, p(99)); !rowsEqual(got, rows) {
		t.Fatalf("limit past end = %v", got)
	}
	// Skip then limit compose to the window [1, 3).
	if got := paginate(rows, p(1), p(2)); !rowsEqual(got, rows[1:3]) {
		t.Fatalf("skip 1 limit 2 = %v", got)
	}
	// A zero limit yields nothing.
	if got := paginate(rows, nil, p(0)); len(got) != 0 {
		t.Fatalf("limit 0 = %v, want empty", got)
	}
}
