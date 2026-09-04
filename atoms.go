package chickpeas

import (
	"sort"
	"sync"
)

// Atoms is the immutable interned-string table of a snapshot: id -> string
// with a reverse index. Atom 0 is always the empty string (the RCPG
// convention: labels, rel types, property keys, and string property values
// are all atom ids, and dense string columns encode missing as atom 0).
// The reverse index is a SORTED PERMUTATION of the ids, not a map: ID()
// runs a leftmost binary search (~20 string compares at a million
// atoms), which is compile-time work -- matchers and constants resolve
// strings once per plan, never per row -- while the map form cost ~50
// bytes per atom resident (45.7 MB of SF1's footprint for its 909k
// atoms, against 3.6 MB for the permutation).
type Atoms struct {
	strings []string
	// sorted holds every atom id ordered by (string, id): binary search
	// by string, then the first hit is the smallest id -- preserving the
	// duplicates-keep-smallest-id contract the map form had. Built
	// LAZILY on the first reverse lookup: sorting ~1M strings costs
	// hundreds of milliseconds, which moved graph LOAD 2.4x when built
	// eagerly (the sweep's LOAD/BI cell, 322 -> 781ms) -- while a
	// loaded-but-never-queried graph never needs the index at all, and
	// the schema-name fast path answers most hot lookups without it.
	sortedOnce sync.Once
	sorted     []uint32
}

// NewAtoms wraps an id-ordered string slice; the reverse index builds on
// first use. When the slice contains duplicates, the smallest id wins
// reverse lookups.
func NewAtoms(strings []string) *Atoms {
	return &Atoms{strings: strings}
}

// sortedIndex builds the reverse permutation once, on first demand.
func (a *Atoms) sortedIndex() []uint32 {
	a.sortedOnce.Do(func() {
		sorted := make([]uint32, len(a.strings))
		for i := range sorted {
			sorted[i] = uint32(i)
		}
		sort.Slice(sorted, func(i, j int) bool {
			x, y := a.strings[sorted[i]], a.strings[sorted[j]]
			if x != y {
				return x < y
			}
			return sorted[i] < sorted[j]
		})
		a.sorted = sorted
	})
	return a.sorted
}

// Len is the number of atoms.
func (a *Atoms) Len() int {
	return len(a.strings)
}

// Resolve returns the string for an atom id; ok is false when out of range.
func (a *Atoms) Resolve(id uint32) (string, bool) {
	if int(id) >= len(a.strings) {
		return "", false
	}
	return a.strings[id], true
}

// ID returns the atom id for a string; ok is false when never interned.
func (a *Atoms) ID(s string) (uint32, bool) {
	sorted := a.sortedIndex()
	i := sort.Search(len(sorted), func(i int) bool {
		return a.strings[sorted[i]] >= s
	})
	if i < len(sorted) && a.strings[sorted[i]] == s {
		return sorted[i], true
	}
	return 0, false
}

// Strings exposes the id-ordered table (for serialization). Callers must
// not mutate it.
func (a *Atoms) Strings() []string {
	return a.strings
}

// Interner is the build-side, thread-safe string interner (the Rust side
// uses lasso). The empty string is pre-interned as atom 0, upholding the
// RCPG convention from the start.
type Interner struct {
	mu      sync.RWMutex
	strings []string
	index   map[string]uint32
}

// NewInterner returns an interner holding only atom 0 = "".
func NewInterner() *Interner {
	return &Interner{
		strings: []string{""},
		index:   map[string]uint32{"": 0},
	}
}

// newInternerFromAtoms seeds a build-side interner with a snapshot's
// id-ordered atom table, preserving every atom id exactly -- the keystone of
// thawing a snapshot back into a builder. Duplicate strings keep the
// smallest id for lookups, matching NewAtoms.
func newInternerFromAtoms(a *Atoms) *Interner {
	strings := make([]string, len(a.strings))
	copy(strings, a.strings)
	if len(strings) == 0 {
		strings = []string{""}
	}
	index := make(map[string]uint32, len(strings))
	for id, s := range strings {
		if _, seen := index[s]; !seen {
			index[s] = uint32(id)
		}
	}
	return &Interner{strings: strings, index: index}
}

// GetOrIntern returns s's atom id, interning it if new.
func (in *Interner) GetOrIntern(s string) uint32 {
	in.mu.RLock()
	id, ok := in.index[s]
	in.mu.RUnlock()
	if ok {
		return id
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if id, ok := in.index[s]; ok {
		return id
	}
	id = uint32(len(in.strings))
	in.strings = append(in.strings, s)
	in.index[s] = id
	return id
}

// Get returns s's atom id without interning; ok is false when unknown.
func (in *Interner) Get(s string) (uint32, bool) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	id, ok := in.index[s]
	return id, ok
}

// Resolve returns the string for an atom id; ok is false when out of range.
func (in *Interner) Resolve(id uint32) (string, bool) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	if int(id) >= len(in.strings) {
		return "", false
	}
	return in.strings[id], true
}

// Len is the number of interned strings (including atom 0).
func (in *Interner) Len() int {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return len(in.strings)
}

// Atoms snapshots the interner into an immutable table.
func (in *Interner) Atoms() *Atoms {
	in.mu.RLock()
	defer in.mu.RUnlock()
	strings := make([]string, len(in.strings))
	copy(strings, in.strings)
	return NewAtoms(strings)
}
