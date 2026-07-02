// RCPG serialization for Snapshot: conversion to and from the rcpg
// package's on-disk data model. Lazily built indexes are derived data and
// are not serialized; they rebuild on first use after reading.

package chickpeas

import (
	"bufio"
	"io"
	"os"
	"sort"

	"github.com/freeeve/gochickpeas/internal/bitset"
	"github.com/freeeve/gochickpeas/nodeset"
	"github.com/freeeve/gochickpeas/rcpg"
)

// computeInToOutFromCSR recomputes the incoming -> outgoing CSR position
// map from the adjacency arrays alone (used when a snapshot is
// reconstructed on deserialization). It pairs the k-th u->v rel of a given
// type in the outgoing CSR with the k-th such rel in the incoming CSR; both
// CSRs preserve insertion order within a group, so the pairing matches the
// original rels.
func computeInToOutFromCSR(outOffsets []uint32, outNbrs []NodeID, outTypes []RelType,
	inOffsets []uint32, inNbrs []NodeID, inTypes []RelType) []uint32 {
	type relKey struct {
		src, dst NodeID
		t        RelType
	}
	n := max(len(inOffsets)-1, 0)
	groups := map[relKey][]uint32{}
	for v := 0; v < n; v++ {
		for inpos := inOffsets[v]; inpos < inOffsets[v+1]; inpos++ {
			key := relKey{src: inNbrs[inpos], dst: NodeID(v), t: inTypes[inpos]}
			groups[key] = append(groups[key], inpos)
		}
	}
	inToOut := make([]uint32, len(outNbrs))
	for u := 0; u < n; u++ {
		for outpos := outOffsets[u]; outpos < outOffsets[u+1]; outpos++ {
			key := relKey{src: NodeID(u), dst: outNbrs[outpos], t: outTypes[outpos]}
			if q := groups[key]; len(q) > 0 {
				inToOut[q[0]] = outpos
				groups[key] = q[1:]
			}
		}
	}
	return inToOut
}

func columnToData(c Column) rcpg.ColumnData {
	switch col := c.(type) {
	case denseI64Col:
		return rcpg.DenseI64(col)
	case denseF64Col:
		return rcpg.DenseF64(col)
	case denseBoolCol:
		return rcpg.DenseBool{Bits: col.bits.ToPackedLSB(), Len: uint32(col.bits.Len())}
	case denseStrCol:
		return rcpg.DenseStr(col)
	case sparseI64Col:
		out := make(rcpg.SparseI64, len(col.ids))
		for i, id := range col.ids {
			out[i] = rcpg.I64Entry{ID: id, Val: col.vals[i]}
		}
		return out
	case sparseF64Col:
		out := make(rcpg.SparseF64, len(col.ids))
		for i, id := range col.ids {
			out[i] = rcpg.F64Entry{ID: id, Val: col.vals[i]}
		}
		return out
	case sparseBoolCol:
		out := make(rcpg.SparseBool, len(col.ids))
		for i, id := range col.ids {
			out[i] = rcpg.BoolEntry{ID: id, Val: col.vals[i]}
		}
		return out
	case sparseStrCol:
		out := make(rcpg.SparseStr, len(col.ids))
		for i, id := range col.ids {
			out[i] = rcpg.StrEntry{ID: id, Val: col.vals[i]}
		}
		return out
	}
	return nil
}

func dataToColumn(data rcpg.ColumnData) Column {
	switch d := data.(type) {
	case rcpg.DenseI64:
		return denseI64Col(d)
	case rcpg.DenseF64:
		return denseF64Col(d)
	case rcpg.DenseBool:
		return denseBoolCol{bits: bitset.FromPackedLSB(d.Bits, int(d.Len))}
	case rcpg.DenseStr:
		return denseStrCol(d)
	case rcpg.SparseI64:
		col := sparseI64Col{ids: make([]uint32, len(d)), vals: make([]int64, len(d))}
		for i, e := range d {
			col.ids[i], col.vals[i] = e.ID, e.Val
		}
		return col
	case rcpg.SparseF64:
		col := sparseF64Col{ids: make([]uint32, len(d)), vals: make([]float64, len(d))}
		for i, e := range d {
			col.ids[i], col.vals[i] = e.ID, e.Val
		}
		return col
	case rcpg.SparseBool:
		col := sparseBoolCol{ids: make([]uint32, len(d)), vals: make([]bool, len(d))}
		for i, e := range d {
			col.ids[i], col.vals[i] = e.ID, e.Val
		}
		return col
	case rcpg.SparseStr:
		col := sparseStrCol{ids: make([]uint32, len(d)), vals: make([]uint32, len(d))}
		for i, e := range d {
			col.ids[i], col.vals[i] = e.ID, e.Val
		}
		return col
	}
	return nil
}

// ToGraphSection converts this snapshot to the plain on-disk data model.
func (g *Snapshot) ToGraphSection() *rcpg.GraphSection {
	return g.graphSectionWith(true, true)
}

// graphSectionWith builds the on-disk model, converting only the requested
// column sets so topology-only writes copy no property data. Index and
// column orders are sorted by atom, as the format requires.
func (g *Snapshot) graphSectionWith(nodeCols, relCols bool) *rcpg.GraphSection {
	labelIndex := make([]rcpg.BitmapEntry, 0, len(g.labelIndex))
	for l, set := range g.labelIndex {
		labelIndex = append(labelIndex, rcpg.BitmapEntry{Atom: l.ID(), Bitmap: set.Bitmap()})
	}
	sort.Slice(labelIndex, func(i, j int) bool { return labelIndex[i].Atom < labelIndex[j].Atom })

	typeIndex := make([]rcpg.BitmapEntry, 0, len(g.typeIndex))
	for t, set := range g.typeIndex {
		typeIndex = append(typeIndex, rcpg.BitmapEntry{Atom: t.ID(), Bitmap: set.Bitmap()})
	}
	sort.Slice(typeIndex, func(i, j int) bool { return typeIndex[i].Atom < typeIndex[j].Atom })

	convert := func(cols map[PropertyKey]Column, enabled bool) []rcpg.Column {
		if !enabled {
			return nil
		}
		out := make([]rcpg.Column, 0, len(cols))
		for key, col := range cols {
			out = append(out, rcpg.Column{Key: key, Data: columnToData(col)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out
	}

	outTypes := make([]uint32, len(g.outTypes))
	for i, t := range g.outTypes {
		outTypes[i] = t.ID()
	}
	inTypes := make([]uint32, len(g.inTypes))
	for i, t := range g.inTypes {
		inTypes[i] = t.ID()
	}

	return &rcpg.GraphSection{
		NNodes:      g.nNodes,
		NRels:       g.nRels,
		OutOffsets:  g.outOffsets,
		OutNbrs:     g.outNbrs,
		OutTypes:    outTypes,
		InOffsets:   g.inOffsets,
		InNbrs:      g.inNbrs,
		InTypes:     inTypes,
		LabelIndex:  labelIndex,
		TypeIndex:   typeIndex,
		NodeColumns: convert(g.columns, nodeCols),
		RelColumns:  convert(g.relColumns, relCols),
		Version:     g.version,
		Atoms:       g.atoms.Strings(),
	}
}

// FromGraphSection builds a snapshot from the on-disk data model. The
// section's slices and bitmaps are taken over; the caller must not reuse
// them.
func FromGraphSection(section *rcpg.GraphSection) *Snapshot {
	g := newSnapshot()
	g.nNodes = section.NNodes
	g.nRels = section.NRels
	g.outOffsets = section.OutOffsets
	g.outNbrs = section.OutNbrs
	g.inOffsets = section.InOffsets
	g.inNbrs = section.InNbrs

	g.outTypes = make([]RelType, len(section.OutTypes))
	for i, t := range section.OutTypes {
		g.outTypes[i] = RelType(t)
	}
	g.inTypes = make([]RelType, len(section.InTypes))
	for i, t := range section.InTypes {
		g.inTypes[i] = RelType(t)
	}

	for _, e := range section.LabelIndex {
		g.labelIndex[Label(e.Atom)] = nodeset.FromBitmap(e.Bitmap)
	}
	for _, e := range section.TypeIndex {
		g.typeIndex[RelType(e.Atom)] = nodeset.FromBitmap(e.Bitmap)
	}
	for _, c := range section.NodeColumns {
		g.columns[c.Key] = dataToColumn(c.Data)
	}
	for _, c := range section.RelColumns {
		g.relColumns[c.Key] = dataToColumn(c.Data)
	}
	g.version = section.Version
	g.atoms = NewAtoms(section.Atoms)

	// The format does not store the incoming -> outgoing position map;
	// rebuild it so incoming rel-property reads are correct. Only needed
	// when the snapshot has rel properties.
	if len(g.relColumns) > 0 {
		g.inToOut = computeInToOutFromCSR(
			g.outOffsets, g.outNbrs, g.outTypes, g.inOffsets, g.inNbrs, g.inTypes)
	}
	return g
}

// WriteRCPG serializes this snapshot to RCPG bytes including property
// columns (see rustychickpeas-format's FORMAT.md for the layout).
func (g *Snapshot) WriteRCPG(w io.Writer) error {
	return g.WriteRCPGWith(w, rcpg.DefaultWriteOptions())
}

// WriteRCPGWith serializes with optional sections per opts;
// TopologyOnlyWriteOptions produces a lean traversal-only file (no property
// columns converted or written).
func (g *Snapshot) WriteRCPGWith(w io.Writer, opts rcpg.WriteOptions) error {
	return rcpg.WriteWith(g.graphSectionWith(opts.NodeColumns, opts.RelColumns), w, opts)
}

// ReadRCPG reads a snapshot from RCPG bytes. Lazy indexes rebuild on first
// use.
func ReadRCPG(b []byte) (*Snapshot, error) {
	section, err := rcpg.Parse(b)
	if err != nil {
		return nil, err
	}
	return FromGraphSection(section), nil
}

// WriteRCPGFile serializes to an RCPG file on disk.
func (g *Snapshot) WriteRCPGFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if err := g.WriteRCPG(w); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadRCPGFile reads a snapshot from an RCPG file on disk.
func ReadRCPGFile(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadRCPG(b)
}
