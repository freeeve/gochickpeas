// Sorted range view over an i64 node property column: the lazily built,
// snapshot-cached primitive behind selective range scans (dates and other
// ordered i64 keys), the ordered complement of the equality property
// index. Entries sort by (value, node id), so any value interval is one
// contiguous window answered by two binary searches and returned as a
// zero-copy id slice.

package chickpeas

import (
	"math"
	"slices"
)

// RangeIndex is a sorted view of an i64 node property column: Window
// answers value intervals with a zero-copy slice of node ids. The zero
// value is an empty index.
type RangeIndex struct {
	vals []int64
	ids  []uint32
}

// ColRangeIndex resolves the sorted range view for a node property key,
// building it on first use and caching it on the snapshot for the
// snapshot's lifetime (the posIndex/LabelDense policy: memory stays
// proportional to the column's entry count, ~12 bytes each). ok is false
// when the key has no node column or the column is not i64.
func (g *Snapshot) ColRangeIndex(key string) (RangeIndex, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return RangeIndex{}, false
	}
	column, ok := g.columns[keyID]
	if !ok || column.Dtype() != DtypeI64 {
		return RangeIndex{}, false
	}
	g.rangeMu.Lock()
	idx, ok := g.rangeIndex[keyID]
	g.rangeMu.Unlock()
	if ok {
		return idx, true
	}
	built := buildRangeIndex(column)
	g.rangeMu.Lock()
	defer g.rangeMu.Unlock()
	if existing, ok := g.rangeIndex[keyID]; ok {
		return existing, true
	}
	g.rangeIndex[keyID] = built
	return built, true
}

// buildRangeIndex collects the column's (value, id) pairs and sorts them
// by value then id, so equal-valued runs enumerate in ascending id order.
func buildRangeIndex(column Column) RangeIndex {
	n := column.Len()
	vals := make([]int64, 0, n)
	ids := make([]uint32, 0, n)
	switch c := column.(type) {
	case denseI64Col:
		for id, v := range c {
			vals = append(vals, v)
			ids = append(ids, uint32(id))
		}
	case sparseI64Col:
		vals = append(vals, c.vals...)
		ids = append(ids, c.ids...)
	default:
		for pos, v := range column.Entries() {
			if x, ok := v.I64(); ok {
				vals = append(vals, x)
				ids = append(ids, pos)
			}
		}
	}
	perm := make([]int, len(vals))
	for i := range perm {
		perm[i] = i
	}
	slices.SortFunc(perm, func(a, b int) int {
		switch {
		case vals[a] != vals[b]:
			if vals[a] < vals[b] {
				return -1
			}
			return 1
		case ids[a] < ids[b]:
			return -1
		case ids[a] > ids[b]:
			return 1
		}
		return 0
	})
	sv := make([]int64, len(vals))
	si := make([]uint32, len(ids))
	for i, p := range perm {
		sv[i], si[i] = vals[p], ids[p]
	}
	return RangeIndex{vals: sv, ids: si}
}

// Len is the number of indexed entries (nodes carrying the key).
func (r RangeIndex) Len() int { return len(r.ids) }

// Window is the node ids whose value lies in the interval [lo, hi] with
// per-bound inclusivity, as a zero-copy slice ordered by (value, id).
// Pass math.MinInt64/math.MaxInt64 with inclusive bounds for a half- or
// unbounded interval. An empty or inverted interval is an empty slice.
func (r RangeIndex) Window(lo, hi int64, loIncl, hiIncl bool) []uint32 {
	start := r.lowerBound(lo, loIncl)
	end := r.upperBound(hi, hiIncl)
	if start >= end {
		return nil
	}
	return r.ids[start:end]
}

// lowerBound is the first slot at or above the lower bound.
func (r RangeIndex) lowerBound(lo int64, incl bool) int {
	if incl {
		i, _ := slices.BinarySearch(r.vals, lo)
		return i
	}
	if lo == math.MaxInt64 {
		return len(r.vals)
	}
	i, _ := slices.BinarySearch(r.vals, lo+1)
	return i
}

// upperBound is the slot just past the upper bound.
func (r RangeIndex) upperBound(hi int64, incl bool) int {
	if !incl {
		i, _ := slices.BinarySearch(r.vals, hi)
		return i
	}
	if hi == math.MaxInt64 {
		return len(r.vals)
	}
	i, _ := slices.BinarySearch(r.vals, hi+1)
	return i
}
