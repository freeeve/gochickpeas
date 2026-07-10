// acc128: a two-word two's-complement sum accumulator, mirroring the core
// package's unexported accumulator of the same shape (kept private in each
// package by design -- the engine's public surface carries a nullable
// int64, not a 128-bit type). Wide enough that an int64 column summed over
// any node count never overflows it, so the fits-int64 verdict depends on
// the true total alone; a transient excursion past int64 range that nets
// back in stays exact.

package exec

import "math/bits"

// acc128 is the 128-bit signed accumulator; the zero value is 0.
type acc128 struct {
	lo uint64
	hi uint64
}

// add folds one int64 in.
func (a *acc128) add(v int64) {
	var carry uint64
	a.lo, carry = bits.Add64(a.lo, uint64(v), 0)
	a.hi += uint64(v>>63) + carry
}

// int64 returns the total and whether it fits int64: the high word must be
// the sign extension of the low word's int64 reading.
func (a acc128) int64() (int64, bool) {
	v := int64(a.lo)
	return v, a.hi == uint64(v>>63)
}

// float64 is the total as a float, for the mixed int/float sum path (the
// float sum's own precision limits apply regardless).
func (a acc128) float64() float64 {
	return float64(int64(a.hi))*0x1p64 + float64(a.lo)
}
