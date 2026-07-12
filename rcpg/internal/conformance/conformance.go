// Package conformance rebuilds the cross-implementation conformance corpus
// defined by rustychickpeas-format's conformance module, using this module's
// own writer. The Rust repo is the corpus's source of truth; these builders
// must stay in lockstep with rustychickpeas-format/src/conformance.rs so the
// reverse-direction interop test (Rust parsing Go-written files) checks real
// from-scratch Go encoding, not a byte copy.
package conformance

import (
	"bytes"
	"encoding/binary"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/rcpg"
)

// Case is one positive conformance case: a graph, how to write it, and
// whether to splice an unknown directory section into the output.
type Case struct {
	Name           string
	Graph          *rcpg.GraphSection
	WriteOptions   rcpg.WriteOptions
	UnknownSection bool
}

// EncodeCase encodes a case exactly as the Rust generator writes it to disk.
func EncodeCase(c Case) ([]byte, error) {
	var buf bytes.Buffer
	if err := rcpg.WriteWith(c.Graph, &buf, c.WriteOptions); err != nil {
		return nil, err
	}
	if c.UnknownSection {
		return spliceUnknownSection(buf.Bytes())
	}
	return buf.Bytes(), nil
}

// Corpus returns the positive cases, mirroring the Rust corpus() builders.
func Corpus() []Case {
	return []Case{
		{
			Name: "empty",
			Graph: &rcpg.GraphSection{
				OutOffsets: []uint32{0},
				InOffsets:  []uint32{0},
				Atoms:      []string{""},
			},
			WriteOptions: rcpg.DefaultWriteOptions(),
		},
		{Name: "small", Graph: smallGraph(), WriteOptions: rcpg.DefaultWriteOptions()},
		{Name: "topology_only", Graph: smallGraph(), WriteOptions: rcpg.TopologyOnlyWriteOptions()},
		{Name: "unknown_section", Graph: smallGraph(), WriteOptions: rcpg.DefaultWriteOptions(), UnknownSection: true},
		{Name: "sparse_ids", Graph: sparseIDsGraph(), WriteOptions: rcpg.DefaultWriteOptions()},
		{Name: "all_columns", Graph: allColumnsGraph(), WriteOptions: rcpg.DefaultWriteOptions()},
		{Name: "multi_label_types", Graph: multiLabelTypesGraph(), WriteOptions: rcpg.DefaultWriteOptions()},
		{Name: "big", Graph: bigGraph(), WriteOptions: rcpg.DefaultWriteOptions()},
		// The small graph re-emitted with the optional section-7 atom
		// index: readers must validate a present index (a malformed one
		// is corrupt -- see the negative corpus) and route by block;
		// writers must reproduce it byte-identically (block_len 1024,
		// offsets relative to the atoms section body, entry 0 = 4).
		{Name: "small_atom_indexed", Graph: smallGraph(), WriteOptions: rcpg.WriteOptions{NodeColumns: true, RelColumns: true, AtomIndex: true}},
	}
}

func bitmap(ids ...uint32) *roaring.Bitmap {
	return roaring.BitmapOf(ids...)
}

// bitmapRange adds ids one by one rather than via AddRange: AddRange builds
// a run container immediately, but the Rust builders collect per-id inserts
// (array/bitmap containers), and byte identity requires matching container
// choices, not just membership.
func bitmapRange(lo, hi uint32) *roaring.Bitmap {
	bm := roaring.New()
	for i := lo; i < hi; i++ {
		bm.Add(i)
	}
	return bm
}

type rel struct{ u, v, t uint32 }

// buildCSR builds both CSR directions + the type index from rels in
// insertion order (the k-th u->v of a type appears k-th in both directions).
func buildCSR(idSpace uint32, rels []rel) *rcpg.GraphSection {
	n := int(idSpace)
	outDeg := make([]uint32, n)
	inDeg := make([]uint32, n)
	for _, r := range rels {
		outDeg[r.u]++
		inDeg[r.v]++
	}
	prefix := func(deg []uint32) []uint32 {
		offs := make([]uint32, n+1)
		for i := range n {
			offs[i+1] = offs[i] + deg[i]
		}
		return offs
	}
	outOffsets, inOffsets := prefix(outDeg), prefix(inDeg)
	m := len(rels)
	outNbrs, outTypes := make([]uint32, m), make([]uint32, m)
	inNbrs, inTypes := make([]uint32, m), make([]uint32, m)
	outNext := append([]uint32(nil), outOffsets[:n]...)
	inNext := append([]uint32(nil), inOffsets[:n]...)
	for _, r := range rels {
		op := outNext[r.u]
		outNbrs[op], outTypes[op] = r.v, r.t
		outNext[r.u]++
		ip := inNext[r.v]
		inNbrs[ip], inTypes[ip] = r.u, r.t
		inNext[r.v]++
	}
	var typeIndex []rcpg.BitmapEntry
	for pos, t := range outTypes {
		found := false
		for i := range typeIndex {
			if typeIndex[i].Atom == t {
				typeIndex[i].Bitmap.Add(uint32(pos))
				found = true
				break
			}
		}
		if !found {
			typeIndex = append(typeIndex, rcpg.BitmapEntry{Atom: t, Bitmap: bitmap(uint32(pos))})
		}
	}
	for i := 1; i < len(typeIndex); i++ {
		for j := i; j > 0 && typeIndex[j-1].Atom > typeIndex[j].Atom; j-- {
			typeIndex[j-1], typeIndex[j] = typeIndex[j], typeIndex[j-1]
		}
	}
	return &rcpg.GraphSection{
		NNodes:     idSpace,
		NRels:      uint64(m),
		OutOffsets: outOffsets,
		OutNbrs:    outNbrs,
		OutTypes:   outTypes,
		InOffsets:  inOffsets,
		InNbrs:     inNbrs,
		InTypes:    inTypes,
		TypeIndex:  typeIndex,
	}
}

func strp(s string) *string { return &s }

// spliceUnknownSection rewrites an RCPG byte stream with one extra directory
// entry (id 99) pointing at an appended junk body, mirroring the Rust
// generator's forward-compatibility case.
func spliceUnknownSection(b []byte) ([]byte, error) {
	junk := []byte("not-a-real-section")
	dir, err := rcpg.ParseDirectory(b)
	if err != nil {
		return nil, err
	}
	entries := dir.Entries()
	count := uint32(len(entries) + 1)
	out := make([]byte, 0, len(b)+24+len(junk))
	out = append(out, rcpg.Magic[:]...)
	out = binary.LittleEndian.AppendUint16(out, rcpg.Version)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, count)
	out = binary.LittleEndian.AppendUint32(out, 0)
	offset := uint64(16 + count*24)
	var bodies [][]byte
	for _, e := range entries {
		out = binary.LittleEndian.AppendUint32(out, e.ID)
		out = binary.LittleEndian.AppendUint32(out, 0)
		out = binary.LittleEndian.AppendUint64(out, offset)
		out = binary.LittleEndian.AppendUint64(out, e.Length)
		bodies = append(bodies, b[e.Offset:e.Offset+e.Length])
		offset += e.Length
	}
	out = binary.LittleEndian.AppendUint32(out, 99)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint64(out, offset)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(junk)))
	for _, body := range bodies {
		out = append(out, body...)
	}
	return append(out, junk...), nil
}
