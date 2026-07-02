// Package rcpg reads and writes RCPG, the RustyChickpeas graph file, and its
// companion RRSR record store (subpackage rrsr). The byte layout is frozen by
// FORMAT.md in the rustychickpeas repository; this implementation is
// conformance-tested byte-for-byte against the Rust codec.
//
// Quick sketch (all integers little-endian):
//
//	header (16 B):  magic "RCPG" | version u16 | flags u16 | section_count u32 | reserved u32
//	directory:      section_count x { id u32, reserved u32, offset u64, length u64 }
//	sections:       at the directory offsets (relative to file start)
//
// Section IDs: 1 atoms, 2 meta, 3 nodes, 4 relationships, 5 node columns,
// 6 relationship columns. Unknown section IDs are ignored on read so the
// format can grow; files with a version above 1 are rejected.
package rcpg

import (
	"bytes"
	"io"
)

// Magic is the RCPG file magic.
var Magic = [4]byte{'R', 'C', 'P', 'G'}

// Version is the RCPG format version this package reads and writes.
const Version uint16 = 1

const (
	sectionAtoms    uint32 = 1
	sectionMeta     uint32 = 2
	sectionNodes    uint32 = 3
	sectionRels     uint32 = 4
	sectionNodeCols uint32 = 5
	sectionRelCols  uint32 = 6
)

const (
	headerLen   = 16
	dirEntryLen = 24
)

// WriteOptions controls which optional sections a writer emits. Topology
// (atoms, meta, nodes, relationships) is always written; property columns
// are optional so large graphs can ship a lean traversal-only file and leave
// per-node data to a range-fetched record store.
type WriteOptions struct {
	NodeColumns bool
	RelColumns  bool
}

// DefaultWriteOptions emits every section.
func DefaultWriteOptions() WriteOptions {
	return WriteOptions{NodeColumns: true, RelColumns: true}
}

// TopologyOnlyWriteOptions emits no property column sections.
func TopologyOnlyWriteOptions() WriteOptions {
	return WriteOptions{}
}

// ParseOptions controls which optional sections a reader materializes.
// Skipped sections cost nothing to parse and nothing in resident memory,
// regardless of whether the file contains them.
type ParseOptions struct {
	NodeColumns bool
	RelColumns  bool
}

// DefaultParseOptions materializes every section.
func DefaultParseOptions() ParseOptions {
	return ParseOptions{NodeColumns: true, RelColumns: true}
}

// TopologyOnlyParseOptions ignores property column sections even when
// present.
func TopologyOnlyParseOptions() ParseOptions {
	return ParseOptions{}
}

// Write serializes a graph to RCPG bytes, including all sections.
func Write(g *GraphSection, w io.Writer) error {
	return WriteWith(g, w, DefaultWriteOptions())
}

// WriteWith serializes a graph to RCPG bytes, emitting optional sections per
// opts. The output is deterministic: section order, and the order of bitmap
// index entries and columns within their sections, follow the GraphSection
// slices exactly (which the spec requires sorted by atom).
func WriteWith(g *GraphSection, w io.Writer, opts WriteOptions) error {
	type section struct {
		id   uint32
		body []byte
	}
	encode := func(id uint32, f func() ([]byte, error)) (section, error) {
		body, err := f()
		return section{id: id, body: body}, err
	}
	sections := make([]section, 0, 6)
	for _, enc := range []struct {
		id      uint32
		f       func() ([]byte, error)
		enabled bool
	}{
		{sectionAtoms, func() ([]byte, error) { return encodeAtoms(g.Atoms) }, true},
		{sectionMeta, func() ([]byte, error) { return encodeMeta(g) }, true},
		{sectionNodes, func() ([]byte, error) { return encodeNodes(g) }, true},
		{sectionRels, func() ([]byte, error) { return encodeRels(g) }, true},
		{sectionNodeCols, func() ([]byte, error) { return encodeColumns(g.NodeColumns) }, opts.NodeColumns},
		{sectionRelCols, func() ([]byte, error) { return encodeColumns(g.RelColumns) }, opts.RelColumns},
	} {
		if !enc.enabled {
			continue
		}
		s, err := encode(enc.id, enc.f)
		if err != nil {
			return err
		}
		sections = append(sections, s)
	}

	var head bytes.Buffer
	head.Write(Magic[:])
	wU16(&head, Version)
	wU16(&head, 0) // flags
	wU32(&head, uint32(len(sections)))
	wU32(&head, 0) // reserved
	offset := uint64(headerLen + len(sections)*dirEntryLen)
	for _, s := range sections {
		wU32(&head, s.id)
		wU32(&head, 0) // reserved
		wU64(&head, offset)
		wU64(&head, uint64(len(s.body)))
		offset += uint64(len(s.body))
	}
	if _, err := w.Write(head.Bytes()); err != nil {
		return err
	}
	for _, s := range sections {
		if _, err := w.Write(s.body); err != nil {
			return err
		}
	}
	return nil
}

// Parse decodes RCPG bytes into a GraphSection, materializing all sections.
// It never panics on malformed input; all structural problems surface as
// errors wrapping ErrCorrupt (or ErrUnsupportedVersion for a future format
// version).
func Parse(b []byte) (*GraphSection, error) {
	return ParseWith(b, DefaultParseOptions())
}

// ParseWith decodes RCPG bytes, materializing optional sections per opts.
// Use TopologyOnlyParseOptions to keep property columns out of memory on
// large graphs.
func ParseWith(b []byte, opts ParseOptions) (*GraphSection, error) {
	dir, err := ParseDirectory(b)
	if err != nil {
		return nil, err
	}
	g := &GraphSection{}
	for _, e := range dir.Entries() {
		end := e.Offset + e.Length
		if end < e.Offset {
			return nil, corruptf("section %d offset+length overflows", e.ID)
		}
		if end > uint64(len(b)) {
			return nil, corruptf("section %d extends past end of file (%d > %d)",
				e.ID, end, len(b))
		}
		if err := decodeSection(e.ID, b[e.Offset:end], opts, g); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// decodeSection decodes one section body into g, honouring opts for the
// optional column sections. Unknown section IDs are ignored (forward
// compatibility). Shared by the eager ParseWith and the lazy ParseLazy paths
// so both decode identically.
func decodeSection(id uint32, body []byte, opts ParseOptions, g *GraphSection) error {
	var err error
	switch {
	case id == sectionAtoms:
		g.Atoms, err = decodeAtoms(body)
	case id == sectionMeta:
		err = decodeMeta(body, g)
	case id == sectionNodes:
		err = decodeNodes(body, g)
	case id == sectionRels:
		err = decodeRels(body, g)
	case id == sectionNodeCols && opts.NodeColumns:
		g.NodeColumns, err = decodeColumns(body)
	case id == sectionRelCols && opts.RelColumns:
		g.RelColumns, err = decodeColumns(body)
	default:
		// Present-but-skipped columns, or forward compatibility: ignore
		// unknown sections.
	}
	return err
}

// sectionWanted reports whether a section is needed under opts: topology
// (atoms/meta/nodes/relationships) always, the property columns only when
// their option is set. Unknown sections are never fetched.
func sectionWanted(id uint32, opts ParseOptions) bool {
	switch id {
	case sectionAtoms, sectionMeta, sectionNodes, sectionRels:
		return true
	case sectionNodeCols:
		return opts.NodeColumns
	case sectionRelCols:
		return opts.RelColumns
	}
	return false
}
