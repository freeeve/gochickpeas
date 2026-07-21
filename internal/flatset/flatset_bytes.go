// Byte-keyed flat structures: a dedup set and a value-carrying map over
// []byte keys that intern into one growable arena probed by an
// open-addressing (offset, length) table -- N distinct keys cost O(log N)
// arena and table growths rather than N heap strings (the per-insert
// string allocation of map[string]... that these replace). Split from
// flatset.go, which holds the integer-keyed twins.
package flatset

import "bytes"

// ByteSet is a dedup set over byte keys that avoids the per-insert string
// allocation of map[string]struct{}: keys append into one growable arena
// probed through an open-addressing table keyed on (offset, length), so N
// distinct keys cost O(log N) arena and table growths rather than N heap
// strings. The first few short keys store inline (a per-group DISTINCT
// set usually holds a handful), spilling into the table form on overflow
// with identical semantics.
type ByteSet struct {
	arena []byte
	slots []bsSlot
	mask  uint32
	count int

	nSmall uint8
	smLen  [4]uint8
	small  [4][24]byte
}

// bsSlot is one open-addressing slot; filled=false marks an empty slot.
type bsSlot struct {
	hash   uint32
	off    uint32
	length uint32
	filled bool
}

// bsHash is FNV-1a over the key bytes.
func bsHash(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// Add reports whether b is newly seen, copying it into the inline store or
// the arena on first sight. The caller may reuse b's backing immediately
// after (the bytes are copied, never aliased).
func (s *ByteSet) Add(b []byte) bool {
	if s.slots == nil {
		for i := 0; i < int(s.nSmall); i++ {
			if int(s.smLen[i]) == len(b) && bytes.Equal(s.small[i][:s.smLen[i]], b) {
				return false
			}
		}
		if int(s.nSmall) < len(s.small) && len(b) <= len(s.small[0]) {
			copy(s.small[s.nSmall][:], b)
			s.smLen[s.nSmall] = uint8(len(b))
			s.nSmall++
			s.count++
			return true
		}
		// Overflow (a fifth key, or one too long for a slot): spill the
		// inline keys into the table form; count already includes them.
		s.slots = make([]bsSlot, 16)
		s.mask = 15
		for i := 0; i < int(s.nSmall); i++ {
			s.insertNew(s.small[i][:s.smLen[i]])
		}
	}
	if s.count*4 >= len(s.slots)*3 {
		s.grow()
	}
	h := bsHash(b)
	i := h & s.mask
	for s.slots[i].filled {
		if sl := s.slots[i]; sl.hash == h && int(sl.length) == len(b) &&
			bytes.Equal(s.arena[sl.off:sl.off+sl.length], b) {
			return false
		}
		i = (i + 1) & s.mask
	}
	off := uint32(len(s.arena))
	s.arena = append(s.arena, b...)
	s.slots[i] = bsSlot{hash: h, off: off, length: uint32(len(b)), filled: true}
	s.count++
	return true
}

// Len is the number of distinct keys added.
func (s *ByteSet) Len() int { return s.count }

// insertNew appends a key known to be absent into the arena and table
// without recounting it (the spill path: inline keys are already counted).
func (s *ByteSet) insertNew(b []byte) {
	h := bsHash(b)
	i := h & s.mask
	for s.slots[i].filled {
		i = (i + 1) & s.mask
	}
	off := uint32(len(s.arena))
	s.arena = append(s.arena, b...)
	s.slots[i] = bsSlot{hash: h, off: off, length: uint32(len(b)), filled: true}
}

// grow doubles the table and rehashes the filled slots.
func (s *ByteSet) grow() {
	old := s.slots
	s.slots = make([]bsSlot, len(old)*2)
	s.mask = uint32(len(s.slots) - 1)
	for _, sl := range old {
		if !sl.filled {
			continue
		}
		i := sl.hash & s.mask
		for s.slots[i].filled {
			i = (i + 1) & s.mask
		}
		s.slots[i] = sl
	}
}

// ByteMap is the value-carrying twin of ByteSet: a byte key maps to an
// int through the same interned arena, so a group-by over a string key
// pays O(log N) growths across N distinct groups instead of one heap
// string per new group.
type ByteMap struct {
	arena []byte
	slots []bmSlot
	mask  uint32
	count int
}

// bmSlot is one open-addressing slot; filled=false marks an empty slot.
type bmSlot struct {
	hash   uint32
	off    uint32
	length uint32
	val    int
	filled bool
}

// GetOrCreate returns the int mapped to b, calling create to mint it on
// first sight (create must not touch this map). b's backing may be reused
// immediately after (the bytes are copied).
func (m *ByteMap) GetOrCreate(b []byte, create func() int) int {
	if m.slots == nil {
		m.slots = make([]bmSlot, 16)
		m.mask = 15
	}
	if m.count*4 >= len(m.slots)*3 {
		m.grow()
	}
	h := bsHash(b)
	i := h & m.mask
	for m.slots[i].filled {
		if sl := m.slots[i]; sl.hash == h && int(sl.length) == len(b) &&
			bytes.Equal(m.arena[sl.off:sl.off+sl.length], b) {
			return sl.val
		}
		i = (i + 1) & m.mask
	}
	v := create()
	off := uint32(len(m.arena))
	m.arena = append(m.arena, b...)
	m.slots[i] = bmSlot{hash: h, off: off, length: uint32(len(b)), val: v, filled: true}
	m.count++
	return v
}

// Get returns the int mapped to b; ok is false when absent.
func (m *ByteMap) Get(b []byte) (int, bool) {
	if m.slots == nil {
		return 0, false
	}
	h := bsHash(b)
	i := h & m.mask
	for m.slots[i].filled {
		if sl := m.slots[i]; sl.hash == h && int(sl.length) == len(b) &&
			bytes.Equal(m.arena[sl.off:sl.off+sl.length], b) {
			return sl.val, true
		}
		i = (i + 1) & m.mask
	}
	return 0, false
}

// Len is the number of distinct keys mapped.
func (m *ByteMap) Len() int { return m.count }

// grow doubles the table and rehashes the filled slots.
func (m *ByteMap) grow() {
	old := m.slots
	m.slots = make([]bmSlot, len(old)*2)
	m.mask = uint32(len(m.slots) - 1)
	for _, sl := range old {
		if !sl.filled {
			continue
		}
		i := sl.hash & m.mask
		for m.slots[i].filled {
			i = (i + 1) & m.mask
		}
		m.slots[i] = sl
	}
}
