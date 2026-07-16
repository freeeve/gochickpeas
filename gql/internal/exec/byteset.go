package exec

import "bytes"

// byteSet is a dedup set over byte keys that avoids the per-insert string
// allocation of map[string]struct{}. A DISTINCT key is built into a reused
// scratch buffer, so map[string(scratch)] = struct{}{} would force a fresh
// immutable string on every distinct row (the lookup is alloc-free, the
// insert is not). Instead the key bytes are appended into one growable arena
// and probed through an open-addressing table keyed on (offset, length) into
// that arena. Probes index the arena by offset, not pointer, so the arena may
// reallocate freely as it grows -- N distinct keys cost O(log N) arena and
// table growths rather than N heap strings.
type byteSet struct {
	arena []byte
	slots []bsSlot
	mask  uint32
	count int
}

// bsSlot is one open-addressing slot; filled=false marks an empty slot (a
// zero-length key never occurs -- every DISTINCT encoding carries a kind tag).
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

// add reports whether b is newly seen, copying it into the arena on first
// sight. The caller may reuse b's backing immediately after (the bytes are
// copied, never aliased).
func (s *byteSet) add(b []byte) bool {
	if s.slots == nil {
		s.slots = make([]bsSlot, 16)
		s.mask = 15
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

// grow doubles the table and rehashes the filled slots.
func (s *byteSet) grow() {
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

// byteMap is the value-carrying twin of byteSet: a byte key maps to an int,
// deduping through the same interned arena so a group-by over a string key
// pays O(log N) growths across N distinct groups instead of one heap string
// per new group. The int payload holds the group's slab index.
type byteMap struct {
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

// getOrCreate returns the int mapped to b, calling create to mint it on first
// sight (create must not touch this map -- typically it appends a new group
// slab). b's backing may be reused immediately after (the bytes are copied).
func (m *byteMap) getOrCreate(b []byte, create func() int) int {
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

// grow doubles the table and rehashes the filled slots.
func (m *byteMap) grow() {
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
