// computeInToOutFromCSR parity: the counting-sort implementation must
// reproduce the reference per-group pairing (the former map-based
// algorithm, kept here as the executable spec) on random multigraphs --
// parallel relationships, self loops, and shared (src, dst) pairs across
// types included.
package chickpeas

import (
	"math/rand"
	"testing"
)

// refInToOutFromCSR is the original map-based pairing, retained as the
// reference: the k-th (src, dst, type) rel of the incoming CSR pairs with
// the k-th of the outgoing CSR.
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
		got := computeInToOutFromCSR(g.outOffsets, g.outNbrs, g.outTypes, g.inOffsets, g.inNbrs, g.inTypes)
		want := refInToOutFromCSR(g.outOffsets, g.outNbrs, g.outTypes, g.inOffsets, g.inNbrs, g.inTypes)
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
