package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestMixedIntFloatSumNegative is the end-to-end regression for task 214: a
// mixed int/float SUM must include a negative int subtotal, not drop it. The
// unguarded acc128.float64() cancelled a small negative total to 0, so
// sum([-5, 2.5]) returned 2.5 instead of -2.5 and sum([-1000, 2.5]) lost the
// -1000 entirely.
func TestMixedIntFloatSumNegative(t *testing.T) {
	b := chickpeas.NewBuilder(1, 0)
	b.AddNode("N")
	g := b.Finalize("sum214")

	floatCase := func(src string, want float64) {
		t.Helper()
		rows, err := RunUncached(g, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("%s: no row", src)
		}
		v, _ := r.GetAt(0)
		got, ok := v.AsFloat()
		if !ok || got != want {
			t.Fatalf("%s = %v (%+v), want %v", src, got, v, want)
		}
	}
	intCase := func(src string, want int64) {
		t.Helper()
		rows, err := RunUncached(g, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("%s: no row", src)
		}
		v, _ := r.GetAt(0)
		got, ok := v.AsInt()
		if !ok || got != want {
			t.Fatalf("%s = %+v, want %d", src, v, want)
		}
	}

	// Mixed int/float with a negative int subtotal -- the bug: was 2.5.
	floatCase("FOR x IN [-5, 2.5] RETURN sum(x) AS s", -2.5)
	// A larger negative that used to vanish entirely -- was 2.5.
	floatCase("FOR x IN [-1000, 2.5] RETURN sum(x) AS s", -997.5)
	// A positive int subtotal was already correct; lock it against a
	// regression of the fix.
	floatCase("FOR x IN [5, 2.5] RETURN sum(x) AS s", 7.5)
	// Pure-int SUM stays on the exact int64 path.
	intCase("FOR x IN [-5, -3] RETURN sum(x) AS s", -8)
}
