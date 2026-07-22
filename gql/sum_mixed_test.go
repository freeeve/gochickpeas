package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
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
		// A mixed int/float SUM is always a Float, even for an integer-valued
		// total -- returning an Int would be a cross-engine result-type
		// divergence with rustychickpeas (task 216).
		if v.Kind() != value.KindFloat {
			t.Fatalf("%s kind = %v, want Float", src, v.Kind())
		}
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
	// An integer-VALUED mixed total still returns Float, not Int (the
	// cross-engine result-type check from task 216); floatCase asserts the
	// Float kind.
	floatCase("FOR x IN [-5, 2.5, 2.5] RETURN sum(x) AS s", 0.0)
	// Pure-int SUM stays on the exact int64 path (and Int type).
	intCase("FOR x IN [-5, -3] RETURN sum(x) AS s", -8)
}
