// Package bitset is a minimal []uint64-backed bit vector: the engine's
// stand-in for Rust's bitvec, backing dense bool columns and the presence
// bitmaps of rank/select columns. Deliberately tiny -- no dependency.
package bitset

import (
	"iter"
	"math/bits"
)

// Bits is a fixed-length bit vector.
type Bits struct {
	words []uint64
	n     int
}

// New returns an all-false bit vector of n bits.
func New(n int) *Bits {
	return &Bits{words: make([]uint64, (n+63)/64), n: n}
}

// FromPackedLSB builds a bit vector from n bits packed LSB-first within each
// byte -- the RCPG dense-bool on-disk layout.
func FromPackedLSB(packed []byte, n int) *Bits {
	b := New(n)
	for i := range n {
		if packed[i/8]&(1<<(i%8)) != 0 {
			b.words[i/64] |= 1 << (i % 64)
		}
	}
	return b
}

// ToPackedLSB returns the bits packed LSB-first within each byte, ceil(n/8)
// bytes -- the RCPG dense-bool on-disk layout.
func (b *Bits) ToPackedLSB() []byte {
	out := make([]byte, (b.n+7)/8)
	for i := range b.n {
		if b.Get(i) {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

// Len is the number of bits.
func (b *Bits) Len() int {
	return b.n
}

// Get reports bit i; false when out of range.
func (b *Bits) Get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(i%64)) != 0
}

// Set sets bit i to v; i must be in [0, Len).
func (b *Bits) Set(i int, v bool) {
	if v {
		b.words[i/64] |= 1 << (i % 64)
	} else {
		b.words[i/64] &^= 1 << (i % 64)
	}
}

// Count is the number of set bits.
func (b *Bits) Count() int {
	c := 0
	for _, w := range b.words {
		c += bits.OnesCount64(w)
	}
	return c
}

// CountRange is the number of set bits in [lo, hi) -- the popcount behind
// rank queries. Out-of-range bounds are clamped.
func (b *Bits) CountRange(lo, hi int) int {
	lo = max(lo, 0)
	hi = min(hi, b.n)
	if lo >= hi {
		return 0
	}
	loW, hiW := lo/64, (hi-1)/64
	loMask := ^uint64(0) << (lo % 64)
	hiMask := ^uint64(0) >> (63 - (hi-1)%64)
	if loW == hiW {
		return bits.OnesCount64(b.words[loW] & loMask & hiMask)
	}
	c := bits.OnesCount64(b.words[loW] & loMask)
	for w := loW + 1; w < hiW; w++ {
		c += bits.OnesCount64(b.words[w])
	}
	return c + bits.OnesCount64(b.words[hiW]&hiMask)
}

// Ones iterates the set bit positions in ascending order.
func (b *Bits) Ones() iter.Seq[int] {
	return func(yield func(int) bool) {
		for wi, w := range b.words {
			for w != 0 {
				i := wi*64 + bits.TrailingZeros64(w)
				if i >= b.n || !yield(i) {
					return
				}
				w &= w - 1
			}
		}
	}
}
