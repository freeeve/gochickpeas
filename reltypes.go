// relTypes stores one relationship type per CSR position. Type values
// are atom ids, so the vector is []RelType in general -- but a graph's
// DISTINCT relationship types almost always fit a byte, and at one
// entry per relationship per direction the wide form was the single
// largest resident structure after the adjacency itself (128.7 MB of
// SF1's 576 MB). When the distinct count fits, the store is a u8
// palette-index vector plus the palette; reads stay O(1) through a
// palette lookup that lives in L1. The representation is chosen by
// VALUE SHAPE (distinct-type count), never by workload.
package chickpeas

// relTypes is the per-position relationship-type vector.
type relTypes struct {
	// wide is the general form; nil when the palette form is active.
	wide []RelType
	// palette[idx[pos]] is the narrow form's read path.
	palette []RelType
	idx     []uint8
}

// relTypePaletteMax is the palette form's capacity: distinct counts
// above this keep the wide form.
const relTypePaletteMax = 256

// At returns the type at CSR position pos.
func (r *relTypes) At(pos uint32) RelType {
	if r.wide != nil {
		return r.wide[pos]
	}
	return r.palette[r.idx[pos]]
}

// Len is the number of positions.
func (r *relTypes) Len() int {
	if r.wide != nil {
		return len(r.wide)
	}
	return len(r.idx)
}

// compressRelTypes builds the narrowest representation of ts: the u8
// palette form when the distinct types fit, else the wide form holding
// ts itself (no copy).
func compressRelTypes(ts []RelType) relTypes {
	// Single pass over a direct-index table (rel-type atoms intern
	// early, so ids are small); see compressRelTypesU32 for the
	// measurement that killed the per-position map form. Huge ids and
	// palette overflow fall back to the wide form, reusing ts.
	maxT := RelType(0)
	for _, t := range ts {
		if t > maxT {
			maxT = t
		}
	}
	if maxT >= denseTypeBound {
		return relTypes{wide: ts}
	}
	lut := make([]int16, int(maxT)+1)
	for i := range lut {
		lut[i] = -1
	}
	var palette []RelType
	idx := make([]uint8, len(ts))
	for pos, t := range ts {
		pi := lut[t]
		if pi < 0 {
			if len(palette) >= relTypePaletteMax {
				return relTypes{wide: ts}
			}
			pi = int16(len(palette))
			lut[t] = pi
			palette = append(palette, t)
		}
		idx[pos] = uint8(pi)
	}
	return relTypes{palette: palette, idx: idx}
}

// denseTypeBound caps the direct-index palette table: a rel-type atom id
// at or past it (never seen in practice -- type names intern before the
// value flood) routes to the wide representation.
const denseTypeBound = 1 << 16

// compressRelTypesU32 is compressRelTypes over the on-disk u32 form,
// building the narrow representation without materializing a wide
// []RelType first (the load path's peak matters).
func compressRelTypesU32(ts []uint32) relTypes {
	// One pass over a direct-index table instead of two passes of map
	// probes: the map form's per-position hashing was 53% of FinBench
	// SF10's load CPU (~670ms of mapaccess/memhash over 26M positions,
	// the sweep's LOAD regression after the storage pair landed).
	maxT := uint32(0)
	for _, t := range ts {
		if t > maxT {
			maxT = t
		}
	}
	wideOut := func() relTypes {
		wide := make([]RelType, len(ts))
		for i, t := range ts {
			wide[i] = RelType(t)
		}
		return relTypes{wide: wide}
	}
	if maxT >= denseTypeBound {
		return wideOut()
	}
	lut := make([]int16, int(maxT)+1)
	for i := range lut {
		lut[i] = -1
	}
	var palette []RelType
	idx := make([]uint8, len(ts))
	for pos, t := range ts {
		pi := lut[t]
		if pi < 0 {
			if len(palette) >= relTypePaletteMax {
				return wideOut()
			}
			pi = int16(len(palette))
			lut[t] = pi
			palette = append(palette, RelType(t))
		}
		idx[pos] = uint8(pi)
	}
	return relTypes{palette: palette, idx: idx}
}

// paletteIndexOf is t's index in r's narrow palette; -1 when absent from
// it or when r is wide (no palette to index).
func paletteIndexOf(r *relTypes, t RelType) int16 {
	if r.wide != nil {
		return -1
	}
	for i, pt := range r.palette {
		if pt == t {
			return int16(i)
		}
	}
	return -1
}

// typeTest hoists a RelMatch's per-position test over one direction's
// type vector: the representation branch, the palette resolution of the
// sought type(s), and the match-all case all resolve ONCE per traversal
// call, leaving the per-relationship work at a u8 (narrow) or u32 (wide)
// load -- the naive m.matches(r.At(k)) form paid the branch and a palette
// dereference on every relationship of every traversal. A snapshot-built
// matcher carries its palette index precomputed on the typed holder, so
// the per-call cost is the closure alone; only a snapshot-less MatchType
// pays a palette scan here. The returned closure captures only slices and
// scalars, so it stays on the stack in a direct loop.
func typeTest(m RelMatch, r *relTypes, out bool) func(pos uint32) bool {
	switch m.kind {
	case 0:
		return func(uint32) bool { return true }
	case 1:
		if r.wide != nil {
			w, t := r.wide, m.one
			return func(pos uint32) bool { return w[pos] == t }
		}
		ti := m.palIndex(r, out)
		if ti < 0 {
			return func(uint32) bool { return false }
		}
		idx, b := r.idx, uint8(ti)
		return func(pos uint32) bool { return idx[pos] == b }
	}
	if len(m.many) == 0 {
		return func(uint32) bool { return false }
	}
	if r.wide == nil {
		// Pre-resolve the sought set into palette-index space: one
		// 256-bit membership word set, then a u8 load + bit test per
		// relationship.
		var mask [4]uint64
		any := false
		for i, pt := range r.palette {
			for _, t := range m.many {
				if pt == t {
					mask[i>>6] |= 1 << (uint(i) & 63)
					any = true
				}
			}
		}
		if !any {
			return func(uint32) bool { return false }
		}
		idx := r.idx
		return func(pos uint32) bool {
			b := idx[pos]
			return mask[b>>6]&(1<<(uint(b)&63)) != 0
		}
	}
	w, many := r.wide, m.many
	return func(pos uint32) bool {
		t := w[pos]
		for _, mt := range many {
			if mt == t {
				return true
			}
		}
		return false
	}
}

// palIndex resolves m.one's palette index for one direction: the holder's
// precomputed value when the matcher came from Snapshot.Match, a palette
// scan for a snapshot-less MatchType. -1 = absent (no rel matches).
func (m RelMatch) palIndex(r *relTypes, out bool) int {
	if m.tp != nil {
		if out {
			return int(m.tp.outPal)
		}
		return int(m.tp.inPal)
	}
	return int(paletteIndexOf(r, m.one))
}

// appendNbrsTyped appends nbrs[k] for each k in [lo, hi) whose type
// matches m, with the representation dispatch hoisted and each variant's
// loop body a direct compare -- no per-relationship indirect call. The
// closure form (typeTest) costs ~2-3ns per relationship in call overhead,
// which dominates below-floor scans that test every relationship of a
// node's mixed run.
func appendNbrsTyped(dst []NodeID, nbrs []NodeID, r *relTypes, m RelMatch, out bool, lo, hi int) []NodeID {
	switch m.kind {
	case 0:
		return append(dst, nbrs[lo:hi]...)
	case 1:
		if w := r.wide; w != nil {
			t := m.one
			for k := lo; k < hi; k++ {
				if w[k] == t {
					dst = append(dst, nbrs[k])
				}
			}
			return dst
		}
		ti := m.palIndex(r, out)
		if ti < 0 {
			return dst
		}
		idx, b := r.idx, uint8(ti)
		for k := lo; k < hi; k++ {
			if idx[k] == b {
				dst = append(dst, nbrs[k])
			}
		}
		return dst
	}
	keep := typeTest(m, r, out)
	for k := lo; k < hi; k++ {
		if keep(uint32(k)) {
			dst = append(dst, nbrs[k])
		}
	}
	return dst
}

// yieldNbrsTyped is appendNbrsTyped's early-stop form: yields matching
// neighbors in CSR order, returning false when yield stopped the
// iteration. The type test stays a direct compare; only matches pay the
// yield call.
func yieldNbrsTyped(nbrs []NodeID, r *relTypes, m RelMatch, out bool, lo, hi int, yield func(NodeID) bool) bool {
	switch m.kind {
	case 0:
		for k := lo; k < hi; k++ {
			if !yield(nbrs[k]) {
				return false
			}
		}
		return true
	case 1:
		if w := r.wide; w != nil {
			t := m.one
			for k := lo; k < hi; k++ {
				if w[k] == t && !yield(nbrs[k]) {
					return false
				}
			}
			return true
		}
		ti := m.palIndex(r, out)
		if ti < 0 {
			return true
		}
		idx, b := r.idx, uint8(ti)
		for k := lo; k < hi; k++ {
			if idx[k] == b && !yield(nbrs[k]) {
				return false
			}
		}
		return true
	}
	keep := typeTest(m, r, out)
	for k := lo; k < hi; k++ {
		if keep(uint32(k)) && !yield(nbrs[k]) {
			return false
		}
	}
	return true
}

// reader hoists At's representation branch: one closure per loop, one
// load (plus a palette lookup on the narrow form) per position.
func (r *relTypes) reader() func(pos uint32) RelType {
	if r.wide != nil {
		w := r.wide
		return func(pos uint32) RelType { return w[pos] }
	}
	idx, palette := r.idx, r.palette
	return func(pos uint32) RelType { return palette[idx[pos]] }
}
