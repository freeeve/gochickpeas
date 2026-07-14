// White-box parity for the run-view bucket hint: the hinted runRange must
// equal a plain full-array binary search for every node id, including
// empty buckets, bucket boundaries, id 0, the last id, and ids past the
// hint's range. The public typed-adjacency differential only routes hub
// nodes through the run view; this pins the lookup itself across the whole
// id space.
package chickpeas

import (
	"math/rand"
	"slices"
	"testing"
)

// refRunRange is the unhinted specification: a binary search over the full
// owner array (the pre-hint implementation).
func refRunRange(nodes []uint32, node NodeID) (int, int) {
	lo, _ := slices.BinarySearch(nodes, uint32(node))
	hi := lo
	for hi < len(nodes) && nodes[hi] == uint32(node) {
		hi++
	}
	return lo, hi
}

func TestRunRangeHintMatchesFullSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// Synthetic single-type CSRs across sizes that cross bucket boundaries:
	// n spanning under one bucket, exactly one, and many; degree skew so
	// some buckets are empty and one is hub-heavy.
	for _, n := range []int{0, 1, 63, 64, 65, 200, 1 << 12} {
		offsets := make([]uint32, n+1)
		var nbrs []NodeID
		var types []RelType
		for u := 0; u < n; u++ {
			offsets[u] = uint32(len(nbrs))
			deg := 0
			switch {
			case u == n/2: // hub: one bucket carries a long run
				deg = 300
			case rng.Intn(3) == 0: // 2/3 of nodes own nothing
				deg = rng.Intn(4)
			}
			for range deg {
				nbrs = append(nbrs, NodeID(rng.Intn(n)))
				types = append(types, RelType(rng.Intn(2))) // ~half type 1
			}
		}
		if n > 0 {
			offsets[n] = uint32(len(nbrs))
		}
		r := buildTypedRuns(offsets, nbrs, types, nil, 1, len(nbrs))
		// Every id in range, plus ids past the hint's coverage.
		for id := 0; id <= n+130; id++ {
			wantLo, wantHi := refRunRange(r.nodes, NodeID(id))
			gotLo, gotHi := r.runRange(NodeID(id))
			// Past-range ids report an empty run; the reference reports the
			// same emptiness at the array end. Only the emptiness is the
			// contract there.
			if id >= n {
				if gotLo != gotHi {
					t.Fatalf("n=%d id=%d: past-range run not empty: [%d,%d)", n, id, gotLo, gotHi)
				}
				continue
			}
			if gotLo != wantLo || gotHi != wantHi {
				t.Fatalf("n=%d id=%d: hinted [%d,%d), reference [%d,%d)", n, id, gotLo, gotHi, wantLo, wantHi)
			}
		}
	}
}
