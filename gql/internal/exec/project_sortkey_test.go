// The packed ORDER BY word encoding must induce exactly the typed
// comparator's order: uint64 comparison of the encoded word equals
// value.TotalOrderF64 on the raw floats (IEEE-754 totalOrder), for every
// pairing of ordinary values and specials, ascending and descending.
package exec

import (
	"math"
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

func TestPackedSortKeyMatchesTotalOrder(t *testing.T) {
	vals := []float64{
		math.Inf(-1), -math.MaxFloat64, -1e18, -2.5, -1, -math.SmallestNonzeroFloat64,
		math.Copysign(0, -1), 0, math.SmallestNonzeroFloat64, 0.5, 1, 2.5, 1e18,
		math.MaxFloat64, math.Inf(1), math.NaN(), -math.NaN(),
		float64(1 << 60), float64(1<<60) + 1, // int64-derived, beyond 2^53
	}
	sign := func(x int) int {
		switch {
		case x < 0:
			return -1
		case x > 0:
			return 1
		}
		return 0
	}
	for _, a := range vals {
		for _, b := range vals {
			want := sign(value.TotalOrderF64(a, b))
			wa, wb := packSortWordF64(a), packSortWordF64(b)
			got := 0
			switch {
			case wa < wb:
				got = -1
			case wa > wb:
				got = 1
			}
			if got != want {
				t.Fatalf("pack order (%v, %v): %d, TotalOrderF64 %d", a, b, got, want)
			}
			// Descending: complemented words must invert the relation.
			da, db := ^wa, ^wb
			gotD := 0
			switch {
			case da < db:
				gotD = -1
			case da > db:
				gotD = 1
			}
			if gotD != -want {
				t.Fatalf("desc pack order (%v, %v): %d, want %d", a, b, gotD, -want)
			}
		}
	}
}
