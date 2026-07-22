package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestDistinctSetEntities covers the entity dedup contract: a node id and a
// relationship position of equal value never conflate (separate id spaces),
// and repeats within a kind are dropped.
func TestDistinctSetEntities(t *testing.T) {
	var d distinctSet
	var scratch []byte
	node := func(id uint32) bool { return d.add(value.Node(chickpeas.NodeID(id)), &scratch) }
	rel := func(pos uint32) bool { return d.add(value.Rel(pos), &scratch) }

	if !node(5) || node(5) {
		t.Fatal("node 5: first newly seen, repeat a duplicate")
	}
	if !node(7) {
		t.Fatal("node 7 is newly seen")
	}
	// A relationship with the same numeric id as a node is distinct.
	if !rel(5) {
		t.Fatal("rel 5 must not conflate with node 5")
	}
	if rel(5) {
		t.Fatal("rel 5 repeat is a duplicate")
	}
}

// TestDistinctSetOverflow drives the inline entity array (8 slots) past its
// capacity so it spills into the probe set, and checks dedup holds across
// the inline/spilled boundary.
func TestDistinctSetOverflow(t *testing.T) {
	var d distinctSet
	var scratch []byte
	const n = 20
	for i := uint32(0); i < n; i++ {
		if !d.add(value.Node(chickpeas.NodeID(i)), &scratch) {
			t.Fatalf("node %d should be newly seen", i)
		}
	}
	for i := uint32(0); i < n; i++ {
		if d.add(value.Node(chickpeas.NodeID(i)), &scratch) {
			t.Fatalf("node %d should be a duplicate after the fill", i)
		}
	}
}

// TestDistinctSetOtherKinds covers the non-entity byte-key store: scalars
// dedup by their kind-tagged key, so equal values collapse and different
// kinds stay distinct.
func TestDistinctSetOtherKinds(t *testing.T) {
	var d distinctSet
	var scratch []byte
	if !d.add(value.Int(3), &scratch) || d.add(value.Int(3), &scratch) {
		t.Fatal("int 3: first newly seen, repeat a duplicate")
	}
	if !d.add(value.Str("a"), &scratch) || d.add(value.Str("a"), &scratch) {
		t.Fatal("str a: first newly seen, repeat a duplicate")
	}
	// Distinct scalars and distinct kinds are all newly seen.
	if !d.add(value.Int(4), &scratch) || !d.add(value.Str("b"), &scratch) || !d.add(value.Bool(true), &scratch) {
		t.Fatal("int 4, str b, bool true are each newly seen")
	}
}

// TestPackedEntityAndGroupKey2 covers the entity group-key packers: the
// single-entity 31-bit pack (kind bit + id, out-of-range declines) and the
// order-sensitive pair form.
func TestPackedEntityAndGroupKey2(t *testing.T) {
	if e, ok := packedEntity30(value.Node(chickpeas.NodeID(5))); !ok || e != 5 {
		t.Fatalf("packedEntity30(node 5) = %d,%v, want 5", e, ok)
	}
	if e, ok := packedEntity30(value.Rel(7)); !ok || e != 1<<30|7 {
		t.Fatalf("packedEntity30(rel 7) = %d,%v", e, ok)
	}
	// Non-entity values and ids at/above 2^30 do not pack.
	if _, ok := packedEntity30(value.Int(3)); ok {
		t.Fatal("int must not pack as an entity")
	}
	if _, ok := packedEntity30(value.Node(chickpeas.NodeID(1 << 30))); ok {
		t.Fatal("id >= 2^30 must not pack")
	}

	// The pair form packs iff both sides pack, deterministically, and is
	// order-sensitive (node,rel differs from rel,node).
	k1, ok := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Rel(7))
	if !ok {
		t.Fatal("node,rel pair must pack")
	}
	if k2, _ := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Rel(7)); k2 != k1 {
		t.Fatal("pair packing must be deterministic")
	}
	if kSwap, _ := packGroupKey2(value.Rel(7), value.Node(chickpeas.NodeID(5))); kSwap == k1 {
		t.Fatal("pair packing must be order-sensitive")
	}
	if _, ok := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Int(3)); ok {
		t.Fatal("a non-entity second operand must not pack")
	}
	if _, ok := packGroupKey2(value.Int(1), value.Node(chickpeas.NodeID(2))); ok {
		t.Fatal("a non-entity first operand must not pack")
	}
}
