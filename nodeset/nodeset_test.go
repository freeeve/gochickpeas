package nodeset_test

import (
	"slices"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/nodeset"
)

func TestBasics(t *testing.T) {
	s := nodeset.New()
	if s.Len() != 0 || !s.IsEmpty() {
		t.Fatal("new set not empty")
	}
	if !s.Insert(1) || !s.Insert(5) {
		t.Fatal("insert of new ids reported existing")
	}
	if s.Insert(1) {
		t.Fatal("re-insert reported newly added")
	}
	if !s.Contains(1) || !s.Contains(5) || s.Contains(3) {
		t.Fatal("membership wrong")
	}
	if !s.Remove(1) || s.Remove(1) {
		t.Fatal("remove semantics wrong")
	}
	if s.Len() != 1 {
		t.Fatalf("len: got %d, want 1", s.Len())
	}
}

func TestMinMaxAsRange(t *testing.T) {
	// Contiguous [5, 9] -> range [5, 10).
	s := nodeset.Of(5, 6, 7, 8, 9)
	if mn, ok := s.Min(); !ok || mn != 5 {
		t.Fatalf("min: got %d/%v", mn, ok)
	}
	if mx, ok := s.Max(); !ok || mx != 9 {
		t.Fatalf("max: got %d/%v", mx, ok)
	}
	if lo, hi, ok := s.AsRange(); !ok || lo != 5 || hi != 10 {
		t.Fatalf("range: got [%d,%d)/%v", lo, hi, ok)
	}

	// A gap breaks contiguity, but min/max still hold.
	sparse := nodeset.Of(5, 6, 7, 9)
	if _, _, ok := sparse.AsRange(); ok {
		t.Fatal("sparse set reported contiguous")
	}

	// Empty set: no min/max, no range.
	empty := nodeset.New()
	if _, ok := empty.Min(); ok {
		t.Fatal("empty set has a min")
	}
	if _, _, ok := empty.AsRange(); ok {
		t.Fatal("empty set reported contiguous")
	}

	// Single element is a one-wide range.
	if lo, hi, ok := nodeset.Of(42).AsRange(); !ok || lo != 42 || hi != 43 {
		t.Fatalf("singleton range: got [%d,%d)/%v", lo, hi, ok)
	}

	// The max-id set cannot express max+1; it must fall back to Iter.
	if _, _, ok := nodeset.Of(^uint32(0)).AsRange(); ok {
		t.Fatal("set holding MaxUint32 reported a range")
	}
}

func TestSetOps(t *testing.T) {
	a := nodeset.Of(1, 2, 3)
	b := nodeset.Of(2, 3, 4)

	and := a.And(b)
	if and.Len() != 2 || !and.Contains(2) || !and.Contains(3) {
		t.Fatalf("and: got %v", and.ToSlice())
	}
	or := a.Or(b)
	if or.Len() != 4 || !or.Contains(1) || !or.Contains(4) {
		t.Fatalf("or: got %v", or.ToSlice())
	}
	sub := a.AndNot(b)
	if sub.Len() != 1 || !sub.Contains(1) {
		t.Fatalf("andnot: got %v", sub.ToSlice())
	}
	// Operands unmutated.
	if a.Len() != 3 || b.Len() != 3 {
		t.Fatal("set op mutated an operand")
	}
}

func TestIter(t *testing.T) {
	s := nodeset.Of(5, 1, 3)
	got := slices.Collect(s.Iter())
	if !slices.Equal(got, []uint32{1, 3, 5}) {
		t.Fatalf("iter: got %v", got)
	}
	// Early break works.
	for range s.Iter() {
		break
	}
}

func TestParFold(t *testing.T) {
	sum := func(s *nodeset.Set) uint64 {
		return nodeset.ParFold(s,
			func() uint64 { return 0 },
			func(acc uint64, id uint32) uint64 { return acc + uint64(id) },
			func(a, b uint64) uint64 { return a + b })
	}
	// Contiguous (range fast path) matches the closed form.
	contiguous := nodeset.New()
	for i := uint32(1); i <= 1000; i++ {
		contiguous.Insert(i)
	}
	if got := sum(contiguous); got != 500500 {
		t.Fatalf("contiguous sum: got %d", got)
	}
	// Sparse (collected fallback path), same arithmetic.
	if got := sum(nodeset.Of(1, 2, 3, 1000)); got != 1006 {
		t.Fatalf("sparse sum: got %d", got)
	}
	// Empty folds to the identity.
	if got := sum(nodeset.New()); got != 0 {
		t.Fatalf("empty sum: got %d", got)
	}
}

// TestOpsMatchRoaringReference ports the Rust differential suite: And/Or/
// AndNot must agree element-for-element with raw roaring ops across small/
// large and low/high-id sets. Deterministic xorshift keeps trials
// reproducible.
func TestOpsMatchRoaringReference(t *testing.T) {
	seed := uint64(0x9E37_79B9_7F4A_7C15)
	rand := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for trial := range 64 {
		hi := uint64(500)
		if trial%2 == 1 {
			hi = 5_000_000
		}
		na := int(rand() % 400)
		nb := int(rand() % 400)
		ra, rb := roaring.New(), roaring.New()
		for range na {
			ra.Add(uint32(rand() % hi))
		}
		for range nb {
			rb.Add(uint32(rand() % hi))
		}
		a := nodeset.FromBitmap(ra.Clone())
		b := nodeset.FromBitmap(rb.Clone())

		check := func(name string, got *nodeset.Set, want *roaring.Bitmap) {
			t.Helper()
			if !slices.Equal(got.ToSlice(), want.ToArray()) {
				t.Fatalf("%s mismatch on trial %d", name, trial)
			}
		}
		check("AND", a.And(b), roaring.And(ra, rb))
		check("OR", a.Or(b), roaring.Or(ra, rb))
		check("SUB", a.AndNot(b), roaring.AndNot(ra, rb))
	}
}

// TestCloneAndEquals pins Clone's independence (mutating the copy leaves the
// original untouched) and Equals's element-for-element comparison across
// equal, unequal, and empty sets.
func TestCloneAndEquals(t *testing.T) {
	orig := nodeset.Of(1, 4, 9)
	clone := orig.Clone()
	if !clone.Equals(orig) {
		t.Fatal("clone should equal its source")
	}
	// The clone is independent: mutating it must not touch the original.
	clone.Insert(100)
	clone.Remove(1)
	if !orig.Contains(1) || orig.Contains(100) {
		t.Fatalf("mutating clone changed original: %v", orig.ToSlice())
	}
	if clone.Equals(orig) {
		t.Fatal("diverged clone should not equal original")
	}

	// Equals is order-independent of construction and true for equal contents.
	if !nodeset.Of(9, 1, 4).Equals(orig) {
		t.Fatal("sets with the same ids should be equal regardless of insert order")
	}
	// Differing cardinality and differing elements are both unequal.
	if nodeset.Of(1, 4).Equals(orig) {
		t.Fatal("subset should not equal superset")
	}
	if nodeset.Of(1, 4, 10).Equals(orig) {
		t.Fatal("same-size sets with a different element should be unequal")
	}
	// Two empty sets are equal; an empty set differs from a non-empty one.
	if !nodeset.New().Equals(nodeset.New()) {
		t.Fatal("two empty sets should be equal")
	}
	if nodeset.New().Equals(orig) {
		t.Fatal("empty set should not equal a non-empty set")
	}
	// Cloning an empty set yields an equal, independent empty set.
	empty := nodeset.New()
	ec := empty.Clone()
	ec.Insert(7)
	if !empty.IsEmpty() {
		t.Fatal("mutating an empty set's clone emptied-set independence broken")
	}
}

// FuzzOpsMatchRoaringReference extends the differential suite to fuzzed id
// sets.
func FuzzOpsMatchRoaringReference(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{2, 3, 4})
	f.Fuzz(func(t *testing.T, rawA, rawB []byte) {
		ra, rb := roaring.New(), roaring.New()
		for i, x := range rawA {
			ra.Add(uint32(i)*257 + uint32(x))
		}
		for i, x := range rawB {
			rb.Add(uint32(i)*263 + uint32(x))
		}
		a, b := nodeset.FromBitmap(ra.Clone()), nodeset.FromBitmap(rb.Clone())
		if !slices.Equal(a.And(b).ToSlice(), roaring.And(ra, rb).ToArray()) {
			t.Fatal("AND mismatch")
		}
		if !slices.Equal(a.Or(b).ToSlice(), roaring.Or(ra, rb).ToArray()) {
			t.Fatal("OR mismatch")
		}
		if !slices.Equal(a.AndNot(b).ToSlice(), roaring.AndNot(ra, rb).ToArray()) {
			t.Fatal("SUB mismatch")
		}
	})
}
