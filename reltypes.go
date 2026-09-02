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
	seen := map[RelType]uint8{}
	for _, t := range ts {
		if _, ok := seen[t]; !ok {
			if len(seen) >= relTypePaletteMax {
				return relTypes{wide: ts}
			}
			seen[t] = uint8(len(seen))
		}
	}
	palette := make([]RelType, len(seen))
	for t, i := range seen {
		palette[i] = t
	}
	idx := make([]uint8, len(ts))
	for pos, t := range ts {
		idx[pos] = seen[t]
	}
	return relTypes{palette: palette, idx: idx}
}

// compressRelTypesU32 is compressRelTypes over the on-disk u32 form,
// building the narrow representation without materializing a wide
// []RelType first (the load path's peak matters).
func compressRelTypesU32(ts []uint32) relTypes {
	seen := map[uint32]uint8{}
	small := true
	for _, t := range ts {
		if _, ok := seen[t]; !ok {
			if len(seen) >= relTypePaletteMax {
				small = false
				break
			}
			seen[t] = uint8(len(seen))
		}
	}
	if !small {
		wide := make([]RelType, len(ts))
		for i, t := range ts {
			wide[i] = RelType(t)
		}
		return relTypes{wide: wide}
	}
	palette := make([]RelType, len(seen))
	for t, i := range seen {
		palette[i] = RelType(t)
	}
	idx := make([]uint8, len(ts))
	for pos, t := range ts {
		idx[pos] = seen[t]
	}
	return relTypes{palette: palette, idx: idx}
}
