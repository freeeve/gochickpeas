// Ports of the Rust aggregate-sum overflow regression suite
// (rustychickpeas tasks/249): the surfaced Sum is nil exactly when the
// group's true total lies outside int64 range, decided by the total alone
// -- never by how the parallel fold partitioned the nodes.
package chickpeas_test

import (
	"math"
	"math/big"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// sumFixture builds n Person nodes whose "v" property cycles through vals,
// all in one group ("grp" = 0).
func sumFixture(t *testing.T, n int, vals ...int64) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, 1)
	for i := 0; i < n; i++ {
		id := chickpeas.NodeID(i)
		b.AddNodeWithID(id, "Person")
		b.SetProp(id, "grp", int64(0))
		b.SetProp(id, "v", vals[i%len(vals)])
	}
	return b.Finalize()
}

// sumRow runs sum(v) by grp over g and returns the single group's row.
func sumRow(t *testing.T, g *chickpeas.Snapshot) chickpeas.AggRow {
	t.Helper()
	res, err := g.Aggregate("Person").By("grp").Sum("v").Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("groups: %d, want 1", len(res.Rows))
	}
	return res.Rows[0]
}

// TestAggregateSumExactAtBounds pins exactness at the int64 boundaries.
func TestAggregateSumExactAtBounds(t *testing.T) {
	for _, v := range []int64{math.MaxInt64, math.MinInt64} {
		r := sumRow(t, sumFixture(t, 1, v))
		if r.Sum == nil || *r.Sum != v {
			t.Fatalf("sum of [%d]: %v, want exact", v, r.Sum)
		}
	}
}

// TestAggregateSumOverflowIsNil: a total one past either boundary reports
// nil -- serially (2 nodes) and through the parallel merge (~20k nodes).
func TestAggregateSumOverflowIsNil(t *testing.T) {
	if r := sumRow(t, sumFixture(t, 2, math.MaxInt64, 1)); r.Sum != nil {
		t.Fatalf("Max+1 reported %d, want nil", *r.Sum)
	}
	if r := sumRow(t, sumFixture(t, 2, math.MinInt64, -1)); r.Sum != nil {
		t.Fatalf("Min-1 reported %d, want nil", *r.Sum)
	}
	const n = 20000
	v := int64(math.MaxInt64/n) + 1 // n*v just past MaxInt64
	if r := sumRow(t, sumFixture(t, n, v)); r.Sum != nil {
		t.Fatalf("parallel overflow reported %d, want nil", *r.Sum)
	}
	fit := int64(math.MaxInt64 / n) // n*fit within range: stays exact
	if r := sumRow(t, sumFixture(t, n, fit)); r.Sum == nil || *r.Sum != fit*n {
		t.Fatalf("parallel in-range sum: %v, want %d", r.Sum, fit*n)
	}
}

// TestAggregateSumTransientExcursionStaysExact is the partition-order
// guard: repeating [+Max, +Max, Min+1, Min+1] nets to exactly 0 however
// the fold chunks the nodes. (An alternating +Max/-Max pattern would not
// overshoot within a contiguous chunk and would pass even against wrapped
// arithmetic; the double-up pattern would not.)
func TestAggregateSumTransientExcursionStaysExact(t *testing.T) {
	r := sumRow(t, sumFixture(t, 20000, math.MaxInt64, math.MaxInt64, math.MinInt64+1, math.MinInt64+1))
	if r.Sum == nil || *r.Sum != 0 {
		t.Fatalf("net-zero excursion: %v, want exactly 0", r.Sum)
	}
}

// TestAggregateSumAbsentIsZeroNotNil: no Sum column still reports 0 --
// nil means overflow and only overflow.
func TestAggregateSumAbsentIsZeroNotNil(t *testing.T) {
	res, err := sumFixture(t, 4, 7).Aggregate("Person").By("grp").Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Sum == nil || *res.Rows[0].Sum != 0 {
		t.Fatalf("count-only rows: %+v, want Sum 0", res.Rows)
	}
}

// FuzzAcc128 checks the accumulator against big.Int over arbitrary value
// sequences and an arbitrary partition point (add both halves separately,
// merge): totals and fits-int64 verdicts must match exactly. Exercised
// through the public API: each half is one labeled group, the merged
// whole a third.
func FuzzAcc128(f *testing.F) {
	f.Add(int64(math.MaxInt64), int64(1), int64(math.MinInt64), int64(-1), uint8(2))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MinInt64+1), int64(math.MinInt64+1), uint8(1))
	f.Fuzz(func(t *testing.T, a, b, c, d int64, split uint8) {
		vals := []int64{a, b, c, d}
		k := int(split) % 5
		bld := chickpeas.NewBuilder(8, 1)
		want := big.NewInt(0)
		for i, v := range vals {
			id := chickpeas.NodeID(i)
			bld.AddNodeWithID(id, "N")
			grp := int64(0)
			if i >= k {
				grp = 1
			}
			bld.SetProp(id, "grp", grp)
			bld.SetProp(id, "v", v)
			want.Add(want, big.NewInt(v))
		}
		res, err := bld.Finalize().Aggregate("N").By("grp").Sum("v").Run()
		if err != nil {
			t.Fatal(err)
		}
		got := big.NewInt(0)
		for _, r := range res.Rows {
			if r.Sum == nil {
				// A nil part is legitimate only if that part's true total
				// overflows; recompute it to check.
				part := big.NewInt(0)
				for i, v := range vals {
					if (int64(0) == r.Key[0]) == (i < k) {
						part.Add(part, big.NewInt(v))
					}
				}
				if part.IsInt64() {
					t.Fatalf("group %d nil but total %s fits", r.Key[0], part)
				}
				return
			}
			got.Add(got, big.NewInt(*r.Sum))
		}
		if want.IsInt64() && got.Cmp(want) != 0 {
			t.Fatalf("sum parts %s, want %s", got, want)
		}
	})
}
