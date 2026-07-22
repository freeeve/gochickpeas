package exec

import (
	"math"
	"testing"
)

// TestAcc128 covers the wide sum accumulator: exact int64 totals with the
// fits-int64 verdict, a total past int64 range read through float64, and a
// transient overflow that nets back to an exact int64.
func TestAcc128(t *testing.T) {
	var z acc128
	if v, ok := z.int64(); v != 0 || !ok {
		t.Fatalf("zero int64 = %d,%v", v, ok)
	}
	if f := z.float64(); f != 0 {
		t.Fatalf("zero float64 = %v", f)
	}

	var a acc128
	a.add(1000)
	a.add(2000)
	if v, ok := a.int64(); v != 3000 || !ok {
		t.Fatalf("sum int64 = %d,%v", v, ok)
	}
	if f := a.float64(); f != 3000 {
		t.Fatalf("sum float64 = %v", f)
	}

	var neg acc128
	neg.add(-5)
	if v, ok := neg.int64(); v != -5 || !ok {
		t.Fatalf("neg int64 = %d,%v", v, ok)
	}
	// A small negative total converts exactly (regression for task 214: the
	// unguarded hi*2^64 + lo form cancelled this to 0, silently dropping the
	// int subtotal of a mixed int/float SUM).
	if f := neg.float64(); f != -5 {
		t.Fatalf("neg float64 = %v, want -5", f)
	}

	// Two MaxInt64 adds overflow int64 but stay exact in the accumulator;
	// float64 reads the wide total (~2^64).
	var big acc128
	big.add(math.MaxInt64)
	big.add(math.MaxInt64)
	if _, ok := big.int64(); ok {
		t.Fatal("2*MaxInt64 must not fit int64")
	}
	if f := big.float64(); f < 1.8e19 || f > 1.9e19 {
		t.Fatalf("wide float64 = %v, want ~1.84e19", f)
	}

	// A transient excursion past int64 that nets back is exact and fits.
	var net acc128
	net.add(math.MaxInt64)
	net.add(math.MaxInt64)
	net.add(-math.MaxInt64)
	net.add(-math.MaxInt64)
	if v, ok := net.int64(); v != 0 || !ok {
		t.Fatalf("netted int64 = %d,%v", v, ok)
	}
	if f := net.float64(); f != 0 {
		t.Fatalf("netted float64 = %v", f)
	}
}
