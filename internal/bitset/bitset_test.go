package bitset_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/internal/bitset"
)

func TestGetSetCount(t *testing.T) {
	b := bitset.New(130)
	for _, i := range []int{0, 63, 64, 65, 127, 128, 129} {
		b.Set(i, true)
	}
	b.Set(65, false)
	want := []int{0, 63, 64, 127, 128, 129}
	if got := slices.Collect(b.Ones()); !slices.Equal(got, want) {
		t.Fatalf("ones: got %v, want %v", got, want)
	}
	if b.Count() != len(want) {
		t.Fatalf("count: got %d, want %d", b.Count(), len(want))
	}
	if b.Get(-1) || b.Get(130) || b.Get(65) {
		t.Fatal("out-of-range or cleared bit reported set")
	}
}

func TestCountRangeMatchesNaive(t *testing.T) {
	// Pseudo-random fill, then compare CountRange against a naive loop for
	// every (lo, hi) pair over a word-boundary-crossing size.
	const n = 200
	b := bitset.New(n)
	seed := uint64(42)
	for i := range n {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		b.Set(i, seed%3 == 0)
	}
	for lo := 0; lo <= n; lo++ {
		for hi := lo; hi <= n; hi++ {
			naive := 0
			for i := lo; i < hi; i++ {
				if b.Get(i) {
					naive++
				}
			}
			if got := b.CountRange(lo, hi); got != naive {
				t.Fatalf("CountRange(%d, %d): got %d, want %d", lo, hi, got, naive)
			}
		}
	}
	if b.CountRange(-5, 500) != b.Count() {
		t.Fatal("clamping wrong")
	}
}

func TestPackedLSBRoundTrip(t *testing.T) {
	// 13 bits (non-multiple of 8) with the conformance corpus's pattern.
	packed := []byte{0b1010_1101, 0b0001_1000}
	b := bitset.FromPackedLSB(packed, 13)
	want := []int{0, 2, 3, 5, 7, 11, 12}
	if got := slices.Collect(b.Ones()); !slices.Equal(got, want) {
		t.Fatalf("ones: got %v, want %v", got, want)
	}
	if got := b.ToPackedLSB(); !bytes.Equal(got, packed) {
		t.Fatalf("repack: got %08b, want %08b", got, packed)
	}
}
