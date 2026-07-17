// Package flatset provides flat open-addressing sets and maps for the
// dedup/membership/grouping structures that dominate allocation-heavy
// inner loops. Go's built-in map allocates a bucket array per doubling
// plus overflow buckets as it fills, and map[K]struct{} insert forces
// per-key work the flat probe tables avoid: every structure here costs
// exactly one slice allocation per doubling. Shared by the gql executor's
// aggregation path and the native benchmark kernels.
package flatset

import "bytes"

// U32Set is an open-addressing set of uint32 ids. Slots store id+1 so
// zero marks empty -- an id of ^uint32(0) cannot occur for array-indexed
// ids (node ids, CSR positions).
type U32Set struct {
	slots []uint32
	mask  uint32
	count int
	// Rec, when set, recycles slot arrays across the sets sharing it:
	// grow returns the outgrown array to the pool and draws replacements
	// from it. Many same-shaped sets climbing the growth ladder together
	// (one DISTINCT set per aggregation group) then churn roughly one
	// array per rung instead of one per set per rung -- the discarded
	// doublings otherwise dominate the aggregation's allocation profile.
	Rec *Recycle
}

// Recycle is a slot-array free list keyed by size class, shared by many
// U32Sets within ONE single-threaded execution (no locking). Arrays are
// zeroed on reuse (the same memclr a fresh make pays), so the saving is
// purely the allocation and its GC/scavenger shadow.
type Recycle struct {
	byLog [26][][]uint32
}

// take returns a zeroed array of exactly n slots (n a power of two), or
// nil when the class is empty.
func (r *Recycle) take(n int) []uint32 {
	if r == nil {
		return nil
	}
	lg := logOf(n)
	free := r.byLog[lg]
	if len(free) == 0 {
		return nil
	}
	a := free[len(free)-1]
	r.byLog[lg] = free[:len(free)-1]
	clear(a)
	return a
}

// put files an outgrown array under its size class.
func (r *Recycle) put(a []uint32) {
	if r == nil || len(a) == 0 {
		return
	}
	lg := logOf(len(a))
	r.byLog[lg] = append(r.byLog[lg], a)
}

// logOf is log2 for the power-of-two slot sizes, clamped into byLog.
func logOf(n int) int {
	lg := 0
	for n > 1 {
		n >>= 1
		lg++
	}
	if lg >= 26 {
		lg = 25
	}
	return lg
}

// Add reports whether id is newly seen (and records it).
func (s *U32Set) Add(id uint32) bool {
	if s.slots == nil {
		if s.slots = s.Rec.take(16); s.slots == nil {
			s.slots = make([]uint32, 16)
		}
		s.mask = 15
	}
	if s.count*4 >= len(s.slots)*3 {
		s.grow()
	}
	k := id + 1
	i := (id * 2654435761) & s.mask
	for s.slots[i] != 0 {
		if s.slots[i] == k {
			return false
		}
		i = (i + 1) & s.mask
	}
	s.slots[i] = k
	s.count++
	return true
}

// Has reports whether id was added.
func (s *U32Set) Has(id uint32) bool {
	if s.slots == nil {
		return false
	}
	k := id + 1
	i := (id * 2654435761) & s.mask
	for s.slots[i] != 0 {
		if s.slots[i] == k {
			return true
		}
		i = (i + 1) & s.mask
	}
	return false
}

// Len is the number of distinct ids added.
func (s *U32Set) Len() int { return s.count }

// Reset empties the set keeping its slot backing, so a reused set's next
// fill allocates nothing until it outgrows its high-water size.
func (s *U32Set) Reset() {
	clear(s.slots)
	s.count = 0
}

// Built reports whether the probe table has been materialized (any Add
// ran) -- callers layering their own inline fast path ahead of the set
// key their spill decision on it.
func (s *U32Set) Built() bool { return s.slots != nil }

func (s *U32Set) grow() {
	old := s.slots
	if s.slots = s.Rec.take(len(old) * 2); s.slots == nil {
		s.slots = make([]uint32, len(old)*2)
	}
	s.mask = uint32(len(s.slots) - 1)
	for _, k := range old {
		if k == 0 {
			continue
		}
		i := ((k - 1) * 2654435761) & s.mask
		for s.slots[i] != 0 {
			i = (i + 1) & s.mask
		}
		s.slots[i] = k
	}
	s.Rec.put(old)
}

// u64Hash is the Fibonacci multiplicative hash over the full key.
func u64Hash(key uint64) uint32 { return uint32(key * 0x9E3779B97F4A7C15 >> 32) }

// U64Set is an open-addressing set of uint64 keys -- the packed-pair
// membership set (e.g. person<<32|forum). Slots store key+1 so zero marks
// empty; the all-ones key cannot occur for packed array-indexed ids.
type U64Set struct {
	slots []uint64
	mask  uint32
	count int
}

// Add reports whether key is newly seen (and records it).
func (s *U64Set) Add(key uint64) bool {
	if s.slots == nil {
		s.slots = make([]uint64, 16)
		s.mask = 15
	}
	if s.count*4 >= len(s.slots)*3 {
		s.grow()
	}
	k := key + 1
	i := u64Hash(key) & s.mask
	for s.slots[i] != 0 {
		if s.slots[i] == k {
			return false
		}
		i = (i + 1) & s.mask
	}
	s.slots[i] = k
	s.count++
	return true
}

// Has reports whether key was added.
func (s *U64Set) Has(key uint64) bool {
	if s.slots == nil {
		return false
	}
	k := key + 1
	i := u64Hash(key) & s.mask
	for s.slots[i] != 0 {
		if s.slots[i] == k {
			return true
		}
		i = (i + 1) & s.mask
	}
	return false
}

// Len is the number of distinct keys added.
func (s *U64Set) Len() int { return s.count }

// Reset empties the set keeping its slot backing, so a reused set's next
// fill allocates nothing until it outgrows its high-water size.
func (s *U64Set) Reset() {
	clear(s.slots)
	s.count = 0
}

func (s *U64Set) grow() {
	old := s.slots
	s.slots = make([]uint64, len(old)*2)
	s.mask = uint32(len(s.slots) - 1)
	for _, k := range old {
		if k == 0 {
			continue
		}
		i := u64Hash(k-1) & s.mask
		for s.slots[i] != 0 {
			i = (i + 1) & s.mask
		}
		s.slots[i] = k
	}
}

// U64Map maps uint64 keys to an int index through an open-addressing
// probe table. Slots carry an explicit used flag, so every key value is
// valid.
type U64Map struct {
	slots []u64Slot
	mask  uint32
	count int
}

// u64Slot is one open-addressing slot.
type u64Slot struct {
	key  uint64
	val  int32
	used bool
}

// GetOrCreate returns the index mapped to key, calling create to mint it
// on first sight (create must not touch this map).
func (m *U64Map) GetOrCreate(key uint64, create func() int) int {
	if m.slots == nil {
		m.slots = make([]u64Slot, 16)
		m.mask = 15
	}
	if m.count*4 >= len(m.slots)*3 {
		m.grow()
	}
	i := u64Hash(key) & m.mask
	for m.slots[i].used {
		if m.slots[i].key == key {
			return int(m.slots[i].val)
		}
		i = (i + 1) & m.mask
	}
	v := create()
	m.slots[i] = u64Slot{key: key, val: int32(v), used: true}
	m.count++
	return v
}

// Get returns the index mapped to key; ok is false when absent.
func (m *U64Map) Get(key uint64) (int, bool) {
	if m.slots == nil {
		return 0, false
	}
	i := u64Hash(key) & m.mask
	for m.slots[i].used {
		if m.slots[i].key == key {
			return int(m.slots[i].val), true
		}
		i = (i + 1) & m.mask
	}
	return 0, false
}

// ForEach visits every (key, index) pair in table order. The visit
// function must not mutate the map.
func (m *U64Map) ForEach(visit func(key uint64, val int)) {
	for _, sl := range m.slots {
		if sl.used {
			visit(sl.key, int(sl.val))
		}
	}
}

// Len is the number of distinct keys mapped.
func (m *U64Map) Len() int { return m.count }

// Reset empties the map keeping its table backing, so a reused map's
// next fill allocates nothing until it outgrows its high-water size.
func (m *U64Map) Reset() {
	clear(m.slots)
	m.count = 0
}

func (m *U64Map) grow() {
	old := m.slots
	m.slots = make([]u64Slot, len(old)*2)
	m.mask = uint32(len(m.slots) - 1)
	for _, sl := range old {
		if !sl.used {
			continue
		}
		i := u64Hash(sl.key) & m.mask
		for m.slots[i].used {
			i = (i + 1) & m.mask
		}
		m.slots[i] = sl
	}
}

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
