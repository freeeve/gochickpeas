// IS NULL / IS NOT NULL over a missing property must return the same answer
// whichever storage layout the column took -- a dense column (high fill) and
// a sparse one (low fill) both report an absent slot as null. Guards against
// the dense-column presence gap reported on the Rust side (rustychickpeas
// task 370 / gochickpeas task 184).
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// fillGraph stages span :N nodes where ids [0,present) carry v="plain"; the
// rest are absent. The present fraction picks the layout (>=80% -> dense str,
// low fill -> sparse).
func fillGraph(t *testing.T, span, present int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(span, 1)
	for i := 0; i < span; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < present; i++ {
		if err := b.SetProp(chickpeas.NodeID(i), "v", "plain"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize()
}

// countScalar runs q (which must RETURN a single count column) and returns it.
func countScalar(t *testing.T, g *chickpeas.Snapshot, q string) int64 {
	t.Helper()
	rows := runBoth(t, g, q)
	r, ok := rows.Next()
	if !ok {
		t.Fatalf("no row from %s", q)
	}
	v, _ := r.GetAt(0)
	n, ok := v.AsInt()
	if !ok {
		t.Fatalf("count column not an int in %s: %v", q, v)
	}
	return n
}

// TestIsNullDenseVsSparse pins that an absent property reads as null at both
// fill rates: the 900-present column finalizes dense str, the 100-present one
// sparse, and IS NULL / IS NOT NULL must agree with the true absent/present
// counts in both (task 184).
func TestIsNullDenseVsSparse(t *testing.T) {
	cases := []struct {
		name          string
		span, present int
		wantNull      int64
		wantNotNull   int64
	}{
		{"dense", 1000, 900, 100, 900},
		{"sparse", 1000, 100, 900, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := fillGraph(t, c.span, c.present)
			gotNull := countScalar(t, g, "MATCH (n:N) WHERE n.v IS NULL RETURN count(*) AS c")
			if gotNull != c.wantNull {
				t.Fatalf("v IS NULL count = %d, want %d", gotNull, c.wantNull)
			}
			gotNotNull := countScalar(t, g, "MATCH (n:N) WHERE n.v IS NOT NULL RETURN count(*) AS c")
			if gotNotNull != c.wantNotNull {
				t.Fatalf("v IS NOT NULL count = %d, want %d", gotNotNull, c.wantNotNull)
			}
		})
	}
}
