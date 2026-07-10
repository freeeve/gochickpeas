// acc128: a two-word two's-complement sum accumulator. Wide enough that an
// int64-valued column summed over any node count the engine can hold never
// overflows it, so the fits-int64 verdict at finalize depends on the true
// total alone -- never on how work was partitioned across goroutines: a
// transient excursion past int64 range that nets back in stays exact.

package chickpeas

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

// merge folds another accumulator in (the parallel partial-merge step).
func (a *acc128) merge(b acc128) {
	var carry uint64
	a.lo, carry = bits.Add64(a.lo, b.lo, 0)
	a.hi += b.hi + carry
}

// int64 returns the total and whether it fits int64: the high word must be
// the sign extension of the low word's int64 reading.
func (a acc128) int64() (int64, bool) {
	v := int64(a.lo)
	return v, a.hi == uint64(v>>63)
}
