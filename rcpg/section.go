// Plain-data model of a serialized graph snapshot and the per-section
// codecs for atoms, meta, nodes (label index), and relationships (CSR +
// type index). Port of the Rust codec's graph.rs plus the section
// encoders/decoders from rcpg.rs.

package rcpg

import (
	"bytes"
	"unicode/utf8"

	"github.com/RoaringBitmap/roaring/v2"
)

// BitmapEntry is one entry of a bitmap index: an atom mapped to a roaring
// bitmap (label atom -> node IDs, or type atom -> outgoing-CSR positions).
type BitmapEntry struct {
	Atom   uint32
	Bitmap *roaring.Bitmap
}

// Column is one property column: a key atom and its storage.
type Column struct {
	Key  uint32
	Data ColumnData
}

// GraphSection is the complete decoded content of an RCPG file.
//
// All string-ish values (labels, relationship types, property keys, string
// property values) are atom IDs into Atoms; atom 0 is always the empty
// string. A nil Version means the meta section carried no version string
// (present-but-empty is distinct on disk).
type GraphSection struct {
	// NNodes is the actual number of distinct nodes, NOT the CSR ID-space
	// size; see OutOffsets.
	NNodes uint32
	// NRels is the number of relationships.
	NRels uint64

	// OutOffsets has csr_id_space + 1 entries. IDs never added have empty
	// ranges, so the ID space can exceed NNodes under sparse IDs.
	OutOffsets []uint32
	// OutNbrs holds destination node IDs, len = NRels.
	OutNbrs []uint32
	// OutTypes holds relationship type atoms, parallel to OutNbrs.
	OutTypes []uint32
	// InOffsets is sized like OutOffsets.
	InOffsets []uint32
	// InNbrs holds source node IDs, len = NRels.
	InNbrs []uint32
	// InTypes holds relationship type atoms, parallel to InNbrs.
	InTypes []uint32

	// LabelIndex maps label atom -> node IDs, sorted by label atom.
	LabelIndex []BitmapEntry
	// TypeIndex maps type atom -> outgoing-CSR relationship positions,
	// sorted by type atom.
	TypeIndex []BitmapEntry

	// NodeColumns are node property columns, sorted by key atom.
	NodeColumns []Column
	// RelColumns are relationship property columns, indexed by the
	// relationship's outgoing-CSR position.
	RelColumns []Column

	// Version is the optional snapshot version string.
	Version *string

	// Atoms is the string table; index 0 is always "".
	Atoms []string
}

// CSRIDSpace is the size of the CSR ID space (max node ID + 1 for non-empty
// graphs).
func (g *GraphSection) CSRIDSpace() uint32 {
	if len(g.OutOffsets) == 0 {
		return 0
	}
	return uint32(len(g.OutOffsets) - 1)
}

// OutNeighbors returns the outgoing neighbors of node, empty for IDs outside
// the ID space.
func (g *GraphSection) OutNeighbors(node uint32) []uint32 {
	return neighbors(g.OutOffsets, g.OutNbrs, node)
}

// InNeighbors returns the incoming neighbors (source node IDs) of node,
// empty for IDs outside the ID space.
func (g *GraphSection) InNeighbors(node uint32) []uint32 {
	return neighbors(g.InOffsets, g.InNbrs, node)
}

func neighbors(offsets, nbrs []uint32, node uint32) []uint32 {
	i := int(node)
	if i+1 >= len(offsets) {
		return nil
	}
	start, end := int(offsets[i]), int(offsets[i+1])
	if start > end || end > len(nbrs) {
		return nil
	}
	return nbrs[start:end]
}

// preallocCap bounds an initial slice reservation by a file-declared element
// count: each element is validated as the decode loop reads it, but the
// up-front reservation happens first, so a crafted count could otherwise
// reserve gigabytes before the loop rejects the truncation.
func preallocCap(count int) int {
	const maxPrealloc = 4096
	return min(count, maxPrealloc)
}

// --- atoms -------------------------------------------------------------------

func encodeAtoms(atoms []string) ([]byte, error) {
	var buf bytes.Buffer
	if err := wLenPrefix(&buf, len(atoms)); err != nil {
		return nil, err
	}
	for _, s := range atoms {
		if err := wString(&buf, s); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func decodeAtoms(body []byte) ([]string, error) {
	c := newCursor(body)
	count, err := c.lenPrefix(1)
	if err != nil {
		return nil, err
	}
	// One backing string serves every atom: substrings share it, so the
	// table costs two allocations instead of one per atom (the dominant
	// residual of the load path at millions of atoms). The backing spans
	// the section tail, so the interleaved length prefixes ride along --
	// a few bytes per atom inside one block that stays alive with the
	// table anyway.
	base := c.pos
	blob := string(c.b[base:])
	atoms := make([]string, 0, preallocCap(count))
	for range count {
		n, err := c.lenPrefix(1)
		if err != nil {
			return nil, err
		}
		s, err := c.take(n)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(s) {
			return nil, corruptf("invalid utf8 at offset %d", c.pos)
		}
		atoms = append(atoms, blob[c.pos-n-base:c.pos-base])
	}
	return atoms, nil
}

// --- meta --------------------------------------------------------------------

func encodeMeta(g *GraphSection) ([]byte, error) {
	var buf bytes.Buffer
	if g.Version != nil {
		wU8(&buf, 1)
		if err := wString(&buf, *g.Version); err != nil {
			return nil, err
		}
	} else {
		wU8(&buf, 0)
	}
	return buf.Bytes(), nil
}

func decodeMeta(body []byte, g *GraphSection) error {
	c := newCursor(body)
	present, err := c.u8()
	if err != nil {
		return err
	}
	if present == 1 {
		v, err := c.str()
		if err != nil {
			return err
		}
		g.Version = &v
	}
	return nil
}

// --- nodes (label index) -------------------------------------------------------

func encodeNodes(g *GraphSection) ([]byte, error) {
	var buf bytes.Buffer
	wU32(&buf, g.NNodes)
	if err := encodeBitmapIndex(&buf, g.LabelIndex); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeNodes(body []byte, g *GraphSection) error {
	c := newCursor(body)
	n, err := c.u32()
	if err != nil {
		return err
	}
	g.NNodes = n
	g.LabelIndex, err = decodeBitmapIndex(c)
	return err
}

// --- relationships (CSR + type index) --------------------------------------------

func encodeRels(g *GraphSection) ([]byte, error) {
	var buf bytes.Buffer
	wU64(&buf, g.NRels)
	for _, v := range [][]uint32{
		g.OutOffsets, g.OutNbrs, g.OutTypes, g.InOffsets, g.InNbrs, g.InTypes,
	} {
		if err := wU32Vec(&buf, v); err != nil {
			return nil, err
		}
	}
	if err := encodeBitmapIndex(&buf, g.TypeIndex); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeRels(body []byte, g *GraphSection) error {
	c := newCursor(body)
	var err error
	if g.NRels, err = c.u64(); err != nil {
		return err
	}
	for _, dst := range []*[]uint32{
		&g.OutOffsets, &g.OutNbrs, &g.OutTypes, &g.InOffsets, &g.InNbrs, &g.InTypes,
	} {
		if *dst, err = c.u32Vec(); err != nil {
			return err
		}
	}
	g.TypeIndex, err = decodeBitmapIndex(c)
	return err
}

// --- bitmap indexes (label/type -> roaring) ----------------------------------------

// Bitmaps use the portable RoaringFormatSpec serialization, interoperable
// with the Rust and C implementations. Go roaring's WriteTo/ReadFrom speak
// exactly that format; the Go-specific "frozen" form is never used.
func encodeBitmapIndex(buf *bytes.Buffer, index []BitmapEntry) error {
	if err := wLenPrefix(buf, len(index)); err != nil {
		return err
	}
	for _, e := range index {
		wU32(buf, e.Atom)
		size := e.Bitmap.GetSerializedSizeInBytes()
		if err := wLenPrefix(buf, int(size)); err != nil {
			return err
		}
		if _, err := e.Bitmap.WriteTo(buf); err != nil {
			return err
		}
	}
	return nil
}

func decodeBitmapIndex(c *cursor) ([]BitmapEntry, error) {
	count, err := c.lenPrefix(8)
	if err != nil {
		return nil, err
	}
	index := make([]BitmapEntry, 0, preallocCap(count))
	for range count {
		atom, err := c.u32()
		if err != nil {
			return nil, err
		}
		n, err := c.lenPrefix(1)
		if err != nil {
			return nil, err
		}
		raw, err := c.take(n)
		if err != nil {
			return nil, err
		}
		bm := roaring.New()
		if _, err := bm.ReadFrom(bytes.NewReader(raw)); err != nil {
			return nil, corruptf("invalid roaring bitmap: %v", err)
		}
		index = append(index, BitmapEntry{Atom: atom, Bitmap: bm})
	}
	return index, nil
}
