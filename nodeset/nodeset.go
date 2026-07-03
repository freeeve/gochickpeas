// Package nodeset provides Set, the engine's node-id set: the substrate
// query results compose through (intersect, union, subtract). Roaring-backed
// behind a private representation; the Rust NodeSet's adaptive dense
// small-set arm can be added later without any API change, so callers must
// never assume the backing.
package nodeset

import (
	"iter"

	"github.com/RoaringBitmap/roaring/v2"
)

// Set is a set of node ids.
type Set struct {
	bm *roaring.Bitmap
}

// New returns an empty set.
func New() *Set {
	return &Set{bm: roaring.New()}
}

// Of returns a set of the given ids.
func Of(ids ...uint32) *Set {
	return &Set{bm: roaring.BitmapOf(ids...)}
}

// FromBitmap wraps a roaring bitmap. The set takes ownership; the caller
// must not mutate bm afterwards.
func FromBitmap(bm *roaring.Bitmap) *Set {
	return &Set{bm: bm}
}

// Bitmap exposes the underlying roaring bitmap for serialization and
// interop. Callers must not mutate it.
func (s *Set) Bitmap() *roaring.Bitmap {
	return s.bm
}

// Clone returns an independent copy.
func (s *Set) Clone() *Set {
	return &Set{bm: s.bm.Clone()}
}

// Len is the number of ids in the set (cardinality).
func (s *Set) Len() int {
	return int(s.bm.GetCardinality())
}

// IsEmpty reports whether the set holds no ids.
func (s *Set) IsEmpty() bool {
	return s.bm.IsEmpty()
}

// Contains reports whether id is in the set.
func (s *Set) Contains(id uint32) bool {
	return s.bm.Contains(id)
}

// Insert adds id, reporting whether it was newly added.
func (s *Set) Insert(id uint32) bool {
	return s.bm.CheckedAdd(id)
}

// Remove deletes id, reporting whether it had been present.
func (s *Set) Remove(id uint32) bool {
	return s.bm.CheckedRemove(id)
}

// Min is the smallest id; ok is false for an empty set.
func (s *Set) Min() (id uint32, ok bool) {
	if s.bm.IsEmpty() {
		return 0, false
	}
	return s.bm.Minimum(), true
}

// Max is the largest id; ok is false for an empty set.
func (s *Set) Max() (id uint32, ok bool) {
	if s.bm.IsEmpty() {
		return 0, false
	}
	return s.bm.Maximum(), true
}

// AsRange reports the set as a contiguous half-open [lo, hi) range, iff it
// is non-empty and gap-free (a set of n ids spanning [min, max] is gap-free
// exactly when max-min+1 == n, so this is O(1)). Callers must keep a correct
// fallback (Iter) for when ok is false.
func (s *Set) AsRange() (lo, hi uint32, ok bool) {
	mn, some := s.Min()
	if !some {
		return 0, 0, false
	}
	mx, _ := s.Max()
	span := uint64(mx) - uint64(mn) + 1
	if span != uint64(s.Len()) || mx == ^uint32(0) {
		return 0, 0, false
	}
	return mn, mx + 1, true
}

// Iter iterates the set's ids in ascending order. The accessor is a
// thin inlinable closure constructor over iterYield, so a direct
// `for range` over it devirtualizes at the call site and the loop body
// compiles as straight code rather than a heap-allocated yield closure.
func (s *Set) Iter() iter.Seq[uint32] {
	return func(yield func(uint32) bool) {
		s.iterYield(yield)
	}
}

// iterYield walks the bitmap in ascending order via the buffered many
// iterator (one 64-id chunk at a time); yield never escapes.
func (s *Set) iterYield(yield func(uint32) bool) {
	it := s.bm.ManyIterator()
	var buf [64]uint32
	for {
		n := it.NextMany(buf[:])
		if n == 0 {
			return
		}
		for _, id := range buf[:n] {
			if !yield(id) {
				return
			}
		}
	}
}

// ToSlice returns the ids in ascending order.
func (s *Set) ToSlice() []uint32 {
	return s.bm.ToArray()
}

// And returns the intersection of s and other; neither operand is mutated.
func (s *Set) And(other *Set) *Set {
	return &Set{bm: roaring.And(s.bm, other.bm)}
}

// Or returns the union of s and other; neither operand is mutated.
func (s *Set) Or(other *Set) *Set {
	return &Set{bm: roaring.Or(s.bm, other.bm)}
}

// AndNot returns the difference s - other; neither operand is mutated.
func (s *Set) AndNot(other *Set) *Set {
	return &Set{bm: roaring.AndNot(s.bm, other.bm)}
}

// Equals reports element-for-element equality.
func (s *Set) Equals(other *Set) bool {
	return s.bm.Equals(other.bm)
}
