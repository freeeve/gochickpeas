// computeInToOutFromCSR parity: the counting-sort implementation must
// reproduce the reference per-group pairing (the former map-based
// algorithm, kept here as the executable spec) on random multigraphs --
// parallel relationships, self loops, and shared (src, dst) pairs across
// types included.
package chickpeas

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/freeeve/gochickpeas/rcpg"
)

// refInToOutFromCSR is the original map-based pairing, retained as the
// reference: the k-th (src, dst, type) rel of the incoming CSR pairs with
// the k-th of the outgoing CSR.
// relTypesWide materializes a vector's wide form for reference-model use.
func relTypesWide(r *relTypes) []RelType {
	out := make([]RelType, r.Len())
	for i := range out {
		out[i] = r.At(uint32(i))
	}
	return out
}

func refInToOutFromCSR(outOffsets []uint32, outNbrs []NodeID, outTypes []RelType,
	inOffsets []uint32, inNbrs []NodeID, inTypes []RelType) []uint32 {
	type relKey struct {
		src, dst NodeID
		t        RelType
	}
	n := max(len(inOffsets)-1, 0)
	groups := map[relKey][]uint32{}
	for v := 0; v < n; v++ {
		for inpos := inOffsets[v]; inpos < inOffsets[v+1]; inpos++ {
			groups[relKey{src: inNbrs[inpos], dst: NodeID(v), t: inTypes[inpos]}] =
				append(groups[relKey{src: inNbrs[inpos], dst: NodeID(v), t: inTypes[inpos]}], inpos)
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

func TestComputeInToOutFromCSRMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	for round := 0; round < 30; round++ {
		n := 1 + rng.Intn(40)
		nRels := rng.Intn(300)
		nTypes := 1 + rng.Intn(4)
		b := NewBuilder(n, max(nRels*2, 1))
		for i := 0; i < n; i++ {
			if _, err := b.AddNode("N"); err != nil {
				t.Fatal(err)
			}
		}
		types := []string{"A", "B", "C", "D"}[:nTypes]
		for i := 0; i < nRels; i++ {
			u, v := NodeID(rng.Intn(n)), NodeID(rng.Intn(n))
			if i%11 == 0 {
				v = u // self loop
			}
			if _, err := b.AddRel(u, v, types[rng.Intn(nTypes)]); err != nil {
				t.Fatal(err)
			}
			if i%7 == 0 { // parallel duplicate, same and different types
				if _, err := b.AddRel(u, v, types[rng.Intn(nTypes)]); err != nil {
					t.Fatal(err)
				}
			}
		}
		g := b.Finalize()
		got := computeInToOutFromCSR(g.outOffsets, g.outNbrs, &g.outTypes, g.inOffsets, g.inNbrs, &g.inTypes)
		want := refInToOutFromCSR(g.outOffsets, g.outNbrs, relTypesWide(&g.outTypes), g.inOffsets, g.inNbrs, relTypesWide(&g.inTypes))
		if len(got) != len(want) {
			t.Fatalf("round %d: len %d vs %d", round, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("round %d: inToOut[%d] = %d, want %d", round, i, got[i], want[i])
			}
		}
		// And against the builder's natively computed mapping, when the
		// finalize path materialized one.
		if len(g.inToOut) == len(got) {
			for i := range got {
				if got[i] != g.inToOut[i] {
					t.Fatalf("round %d: inToOut[%d] = %d, builder has %d", round, i, got[i], g.inToOut[i])
				}
			}
		}
	}
}

// TestToGraphSectionRoundTrip covers ToGraphSection (the public snapshot ->
// on-disk model converter) by round-tripping a small graph -- nodes, a label,
// rels, and both a node and a rel property column -- through FromGraphSection
// and asserting the reload preserves topology and property values.
func TestToGraphSectionRoundTrip(t *testing.T) {
	b := NewBuilder(4, 4)
	for i := 0; i < 4; i++ {
		if _, err := b.AddNode("Person"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(NodeID(i), "age", int64(20+i)); err != nil {
			t.Fatal(err)
		}
	}
	// A 0->1->2->3 chain with a weight rel column.
	for i := 0; i < 3; i++ {
		idx, err := b.AddRel(NodeID(i), NodeID(i+1), "KNOWS")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(idx, "w", float64(i)+0.25); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("age", "w")

	section := g.ToGraphSection()
	if section.NNodes != 4 {
		t.Fatalf("section NNodes = %d, want 4", section.NNodes)
	}
	if section.NRels != 3 {
		t.Fatalf("section NRels = %d, want 3", section.NRels)
	}
	if len(section.NodeColumns) == 0 || len(section.RelColumns) == 0 {
		t.Fatalf("section columns empty: node=%d rel=%d", len(section.NodeColumns), len(section.RelColumns))
	}

	// The section reloads to an equivalent snapshot.
	g2 := FromGraphSection(section)
	if g2.NodeCount() != g.NodeCount() || g2.RelCount() != g.RelCount() {
		t.Fatalf("reload counts = (%d nodes, %d rels), want (%d, %d)",
			g2.NodeCount(), g2.RelCount(), g.NodeCount(), g.RelCount())
	}
	if v, ok := g2.Prop(NodeID(2), "age").I64(); !ok || v != 22 {
		t.Fatalf("reload age[2] = %d/%v, want 22", v, ok)
	}
}

// TestSparseExistenceRoundTrip covers the section-8 existence bitmap
// (task 328): written WITH the option, a sparse graph's gaps survive the
// round trip (NodeExists answers exactly the built set); written with
// the DEFAULT options the section is absent and the reader keeps the
// legacy every-in-space-id presumption -- and dense graphs never emit
// the section at all, so their bytes are unchanged (the conformance
// corpus pins that separately).
func TestSparseExistenceRoundTrip(t *testing.T) {
	b := NewBuilder(8, 0)
	for _, id := range []uint32{0, 7, 1000, 5000} {
		if _, err := b.AddNodeWithID(id, "Thing"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	// Opt-in write: existence survives.
	var buf bytes.Buffer
	opts := rcpg.DefaultWriteOptions()
	opts.Existence = true
	if err := g.WriteRCPGWith(&buf, opts); err != nil {
		t.Fatal(err)
	}
	rt, err := ReadRCPG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if rt.NodeCount() != 4 {
		t.Fatalf("round-trip node count = %d, want 4", rt.NodeCount())
	}
	for _, id := range []uint32{0, 7, 1000, 5000} {
		if !rt.NodeExists(NodeID(id)) {
			t.Fatalf("real node %d lost across the round trip", id)
		}
	}
	for _, id := range []uint32{1, 6, 999, 4999} {
		if rt.NodeExists(NodeID(id)) {
			t.Fatalf("gap id %d exists after the round trip", id)
		}
	}

	// Default write: no section, legacy presumption on read.
	var legacy bytes.Buffer
	if err := g.WriteRCPG(&legacy); err != nil {
		t.Fatal(err)
	}
	lt, err := ReadRCPG(legacy.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !lt.NodeExists(NodeID(1)) {
		t.Fatal("legacy read must presume in-space ids exist (no existence section)")
	}

	// A dense graph never emits the section even when asked.
	db := NewBuilder(8, 0)
	for range 4 {
		if _, err := db.AddNode("Thing"); err != nil {
			t.Fatal(err)
		}
	}
	dg := db.Finalize()
	var d1, d2 bytes.Buffer
	if err := dg.WriteRCPG(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dg.WriteRCPGWith(&d2, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d1.Bytes(), d2.Bytes()) {
		t.Fatal("dense write with the existence option changed bytes")
	}
}
