package chickpeas_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// present reports whether a Prop read resolved to a value.
func present(p chickpeas.Prop) bool {
	_, ok := p.Value()
	return ok
}

// TestRemovePropSweepsAllStagedWrites: property removal must clear every
// staged occurrence -- duplicate writes in one typed column and stagings
// under several value types -- or a stale value resurrects at Finalize.
func TestRemovePropSweepsAllStagedWrites(t *testing.T) {
	cases := []struct {
		name  string
		stage func(b *chickpeas.Builder)
	}{
		{"duplicate writes one type", func(b *chickpeas.Builder) {
			b.SetProp(0, "x", int64(1))
			b.SetProp(0, "x", int64(2))
		}},
		{"same key two types", func(b *chickpeas.Builder) {
			b.SetProp(0, "x", int64(1))
			b.SetProp(0, "x", "one")
		}},
		{"all four types", func(b *chickpeas.Builder) {
			b.SetProp(0, "x", int64(1))
			b.SetProp(0, "x", 1.5)
			b.SetProp(0, "x", true)
			b.SetProp(0, "x", "one")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := chickpeas.NewBuilder(0, 0)
			b.AddNodeWithID(0, "L")
			b.AddNodeWithID(9, "L") // widen the span so columns stay sparse
			tc.stage(b)
			if !b.RemoveProp(0, "x") {
				t.Fatal("RemoveProp reported nothing removed")
			}
			if _, ok := b.Prop(0, "x"); ok {
				t.Fatal("staged value survived removal")
			}
			g := b.Finalize()
			if present(g.Prop(0, "x")) {
				t.Fatal("removed property resurrected at Finalize")
			}
		})
	}

	t.Run("unknown key or node", func(t *testing.T) {
		b := chickpeas.NewBuilder(0, 0)
		b.AddNodeWithID(0, "L")
		b.SetProp(0, "x", int64(1))
		if b.RemoveProp(0, "never") {
			t.Fatal("unknown key reported removed")
		}
		if b.RemoveProp(5, "x") {
			t.Fatal("unknown node reported removed")
		}
		if v, _ := b.Prop(0, "x"); v != chickpeas.I64Value(1) {
			t.Fatal("unrelated removal disturbed the staged value")
		}
	})
}

// TestUpdatePropLastWriteWinsAcrossTypes: a key restaged under a new value
// type must not leave the old type's staging behind -- Finalize's per-type
// column loops would resolve the duplicate by loop order (str > bool > f64 >
// i64), not write order.
func TestUpdatePropLastWriteWinsAcrossTypes(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(9, "L")
	// str staged first would win the loop-order tie; the update to i64 must
	// still take effect.
	b.SetProp(0, "x", "stale")
	if err := b.UpdateProp(0, "x", int64(7)); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()
	if v, ok := g.Prop(0, "x").I64(); !ok || v != 7 {
		t.Fatalf("cross-type update lost: got (%d, %v), want 7", v, ok)
	}
	if _, ok := g.Prop(0, "x").Str(); ok {
		t.Fatal("stale str column survived a cross-type update")
	}
}

// TestRemoveRelTombstones: rel removal with parallel rels and rel props --
// indexes handed out by AddRel stay valid, the tombstoned rel and its props
// drop at Finalize, and (u, v, type) addressing resolves to the first
// surviving rel.
func TestRemoveRelTombstones(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(1, "L")
	idx := make([]int, 3)
	for i := range idx {
		var err error
		if idx[i], err = b.AddRel(0, 1, "DUP"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(idx[i], "n", int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.RemoveRel(idx[1]); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveRel(idx[1]); !errors.Is(err, chickpeas.ErrRelNotFound) {
		t.Fatalf("double remove: %v", err)
	}
	if err := b.RemoveRel(99); !errors.Is(err, chickpeas.ErrRelNotFound) {
		t.Fatalf("out of range remove: %v", err)
	}
	if err := b.SetRelPropAt(idx[1], "n", int64(9)); !errors.Is(err, chickpeas.ErrRelNotFound) {
		t.Fatalf("prop set on removed rel: %v", err)
	}
	if _, err := b.RemoveRelPropAt(idx[1], "n"); !errors.Is(err, chickpeas.ErrRelNotFound) {
		t.Fatalf("prop remove on removed rel: %v", err)
	}
	// Indexes handed out before the removal still address their rels.
	if err := b.SetRelPropAt(idx[2], "n", int64(30)); err != nil {
		t.Fatal(err)
	}
	if b.RelCount() != 2 {
		t.Fatalf("staged live rel count: %d", b.RelCount())
	}
	if got := b.NeighborIDs(0, chickpeas.Outgoing); !slices.Equal(got, []chickpeas.NodeID{1, 1}) {
		t.Fatalf("staged neighbors skip the tombstone: %v", got)
	}

	g := b.Finalize()
	if g.RelCount() != 2 {
		t.Fatalf("finalized rel count: %d", g.RelCount())
	}
	var vals []int64
	for r := range g.Rels(0, chickpeas.Outgoing) {
		vals = append(vals, g.RelProp(r.Pos, "n").I64Or(-1))
	}
	if !slices.Equal(vals, []int64{1, 30}) {
		t.Fatalf("surviving rel props: %v, want [1 30]", vals)
	}
}

// TestRemoveRelProp: targeted rel-prop removal by (u, v, type) and by index,
// leaving the rel itself and sibling props in place.
func TestRemoveRelProp(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(1, "L")
	if _, err := b.AddRel(0, 1, "R"); err != nil {
		t.Fatal(err)
	}
	b.SetRelProp(0, 1, "R", "w", 1.5)
	b.SetRelProp(0, 1, "R", "keep", int64(2))
	if removed, err := b.RemoveRelProp(0, 1, "R", "w"); err != nil || !removed {
		t.Fatalf("staged prop removal: removed=%v err=%v", removed, err)
	}
	if _, err := b.RemoveRelProp(0, 9, "R", "w"); !errors.Is(err, chickpeas.ErrRelNotFound) {
		t.Fatalf("missing rel: %v", err)
	}
	// A key with no staged pair on the rel is a reported no-op -- (false,
	// nil), distinguishable from both a real removal and a bad handle.
	if removed, err := b.RemoveRelPropAt(0, "never-staged"); err != nil || removed {
		t.Fatalf("unknown key must be a reported no-op: removed=%v err=%v", removed, err)
	}
	if removed, err := b.RemoveRelPropAt(0, "w"); err != nil || removed {
		t.Fatalf("already-removed key must be a reported no-op: removed=%v err=%v", removed, err)
	}
	g := b.Finalize()
	if present(g.RelProp(0, "w")) {
		t.Fatal("removed rel prop resurrected")
	}
	if v := g.RelProp(0, "keep").I64Or(-1); v != 2 {
		t.Fatalf("sibling prop: %d", v)
	}
}

// TestRemoveNodeDetachDelete: detach-delete drops the node's labels and
// props immediately and cascades to incident rels (both directions, with
// their rel props) at Finalize; untouched nodes and rels survive.
func TestRemoveNodeDetachDelete(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	for id := range chickpeas.NodeID(4) {
		b.AddNodeWithID(id, "L")
	}
	b.AddRel(0, 1, "R") // dies: 1 is removed (incoming side)
	b.AddRel(1, 2, "R") // dies: 1 is removed (outgoing side)
	b.AddRel(2, 3, "R") // survives
	b.SetRelPropAt(1, "w", int64(12))
	b.SetRelPropAt(2, "w", int64(23))
	b.SetProp(1, "name", "doomed")
	if !b.RemoveNode(1) {
		t.Fatal("known node not removed")
	}
	if b.RemoveNode(1) {
		t.Fatal("double remove reported true")
	}
	if b.RemoveNode(99) {
		t.Fatal("unknown node reported removed")
	}
	if got := b.NodeLabels(1); len(got) != 0 {
		t.Fatalf("labels survived removal: %v", got)
	}
	if _, ok := b.Prop(1, "name"); ok {
		t.Fatal("prop survived removal")
	}
	if b.NodeCount() != 3 || b.RelCount() != 1 {
		t.Fatalf("staged counts after removal: %d nodes / %d rels", b.NodeCount(), b.RelCount())
	}

	g := b.Finalize()
	if g.NodeCount() != 3 || g.RelCount() != 1 {
		t.Fatalf("finalized counts: %d nodes / %d rels", g.NodeCount(), g.RelCount())
	}
	if g.HasLabel(1, "L") {
		t.Fatal("removed node kept its label")
	}
	if got := slices.Collect(g.Neighbors(2, chickpeas.Outgoing)); !slices.Equal(got, []uint32{3}) {
		t.Fatalf("surviving rel: %v", got)
	}
	if got := slices.Collect(g.Neighbors(0, chickpeas.Outgoing)); len(got) != 0 {
		t.Fatalf("cascaded rel survived: %v", got)
	}
	for r := range g.Rels(2, chickpeas.Outgoing) {
		if v := g.RelProp(r.Pos, "w").I64Or(-1); v != 23 {
			t.Fatalf("surviving rel prop: %d (rel-prop remap leaked a dead pair?)", v)
		}
	}
}

// TestRemoveNodeShrinksIDSpace: removing the maximum node shrinks the CSR id
// span (Finalize sizes from knownNodes.Maximum()+1), and nextNodeID never
// rewinds -- ids retire, they are not reused.
func TestRemoveNodeShrinksIDSpace(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(500, "L")
	b.RemoveNode(500)
	g := b.Finalize()
	if g.CSRIDSpace() != 1 || g.NodeCount() != 1 {
		t.Fatalf("id space after removing max: %d span / %d nodes", g.CSRIDSpace(), g.NodeCount())
	}

	b2 := chickpeas.NewBuilder(0, 0)
	b2.AddNodeWithID(500, "L")
	b2.RemoveNode(500)
	if id, _ := b2.AddNode("L"); id != 501 {
		t.Fatalf("auto id reused a retired id: %d", id)
	}
}

// TestRemoveNodeResurrection: a staging touch after removal resurrects the
// id as an unlabeled, propertyless node -- the new rel survives Finalize
// while everything staged before the removal stays dead.
func TestRemoveNodeResurrection(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(1, "L")
	b.AddNodeWithID(2, "L")
	b.AddRel(0, 1, "OLD")
	b.SetProp(1, "name", "gone")
	b.RemoveNode(1)
	// Resurrection: the endpoint auto-registers, so the new rel must live.
	if _, err := b.AddRel(2, 1, "NEW"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()
	if g.NodeCount() != 3 || g.RelCount() != 1 {
		t.Fatalf("counts: %d nodes / %d rels", g.NodeCount(), g.RelCount())
	}
	if got := slices.Collect(g.Neighbors(2, chickpeas.Outgoing)); !slices.Equal(got, []uint32{1}) {
		t.Fatalf("resurrecting rel: %v", got)
	}
	if got := slices.Collect(g.Neighbors(0, chickpeas.Outgoing)); len(got) != 0 {
		t.Fatalf("pre-removal rel resurrected: %v", got)
	}
	if g.HasLabel(1, "L") {
		t.Fatal("resurrected node kept its old label")
	}
	if present(g.Prop(1, "name")) {
		t.Fatal("resurrected node kept its old property")
	}
}

// TestRemovalCrossesDenseThreshold: a column staged dense (>= 80% fill) must
// re-select sparse storage when removals drop it below the threshold -- and
// the removed nodes then read absent, not present-as-zero.
func TestRemovalCrossesDenseThreshold(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	for id := range chickpeas.NodeID(10) {
		b.AddNodeWithID(id, "L")
		b.SetProp(id, "x", int64(id)+100)
	}
	for id := chickpeas.NodeID(1); id < 5; id++ {
		if !b.RemoveProp(id, "x") {
			t.Fatalf("remove %d", id)
		}
	}
	// 6 of 10 staged writes remain: below the 80% threshold.
	g := b.Finalize()
	c, ok := g.Col("x")
	if !ok {
		t.Fatal("column missing")
	}
	if _, dense := c.I64().Slice(); dense {
		t.Fatal("column stayed dense below the fill threshold")
	}
	if present(g.Prop(3, "x")) {
		t.Fatal("removed value still present (present-as-zero leak)")
	}
	if v := g.Prop(7, "x").I64Or(-1); v != 107 {
		t.Fatalf("surviving value: %d", v)
	}
}

// TestRemovalThawInteraction: removals applied to a thawed builder behave
// exactly like removals on a fresh one -- including removal of thaw-staged
// props and rels, and a second thaw of the edited snapshot.
func TestRemovalThawInteraction(t *testing.T) {
	g := fixture(t, "multi_label_types")
	b := chickpeas.NewBuilderFromSnapshot(g)

	// Kill one of the parallel (1)-[:DUP]->(2) rels via detach-deleting
	// nothing -- address it directly. Staged order is a linear extension of
	// the CSRs; find a DUP rel by (u, v, type) addressing.
	if removed, err := b.RemoveRelProp(1, 2, "OTHER", "OTHER"); err != nil || !removed {
		t.Fatalf("thaw-staged prop removal: removed=%v err=%v", removed, err)
	}
	if !b.RemoveProp(0, "never-there") == false {
		t.Fatal("unexpected removal")
	}
	if !b.RemoveNode(0) { // detach-deletes the self-loop
		t.Fatal("remove node 0")
	}
	g2 := b.Finalize()
	if g2.NodeCount() != 2 || g2.RelCount() != 4 {
		t.Fatalf("counts: %d nodes / %d rels", g2.NodeCount(), g2.RelCount())
	}
	// The pair removed above is gone: at 3-of-4 fill the column refinalizes
	// sparse (full coverage is required for dense since tasks/041), so the
	// removed position reads absent -- a removal is now observable
	// regardless of the surrounding fill ratio.
	for r := range g2.Rels(1, chickpeas.Outgoing, "OTHER") {
		if v, ok := g2.RelProp(r.Pos, "OTHER").I64(); ok {
			t.Fatalf("removed rel prop survived refinalize: %d", v)
		}
	}
	// DUP rels keep theirs (originals 2, 3, and 5 by staging order).
	var kept []int64
	for _, n := range []chickpeas.NodeID{1, 2} {
		for r := range g2.Rels(n, chickpeas.Outgoing, "DUP") {
			kept = append(kept, g2.RelProp(r.Pos, "OTHER").I64Or(-1))
		}
	}
	if !slices.Equal(kept, []int64{2, 3, 5}) {
		t.Fatalf("unrelated rel props disturbed: %v", kept)
	}

	// Thaw the edited snapshot again and refinalize untouched: a fixed point.
	g3 := chickpeas.NewBuilderFromSnapshot(g2).Finalize()
	if g3.NodeCount() != g2.NodeCount() || g3.RelCount() != g2.RelCount() {
		t.Fatal("second thaw drifted")
	}
}

// TestEmptiedStagedKeyLeavesNoTrace pins the emptied-key invariant the
// Rust port tripped over while mirroring this surface (task 075, their
// 223 phase 2): a sweep that empties a staged pair vec must delete the
// map entry, never leave an empty one -- a ghost entry could finalize an
// empty column or, in a codec with numeric unification, widen a same-key
// survivor to float. Pinned here as: no column artifact survives an
// all-pairs removal, and an int survivor under a key whose float staging
// was emptied stays an int. (The written files of a staged-then-emptied
// builder and a never-staged twin differ only by the retained interner
// atom -- a documented build-history artifact, checked structurally.)
func TestEmptiedStagedKeyLeavesNoTrace(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	n0, _ := b.AddNode("N")
	n1, _ := b.AddNode("N")
	r, err := b.AddRel(n0, n1, "R")
	if err != nil {
		t.Fatal(err)
	}
	// Int survivor on n0; a float staged under the SAME key on n1 and
	// then removed -- the emptied f64 entry must not shadow or widen it.
	if err := b.SetProp(n0, "k", int64(7)); err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(n1, "k", 2.5); err != nil {
		t.Fatal(err)
	}
	if !b.RemoveProp(n1, "k") {
		t.Fatal("RemoveProp missed a staged pair")
	}
	// A rel key whose only carrier is tombstoned: no column may survive.
	if err := b.SetRelPropAt(r, "rk", int64(9)); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveRel(r); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()

	if v, ok := g.Prop(n0, "k").I64(); !ok || v != 7 {
		t.Fatalf("k on n0 = (%v, %v), want int 7 (survivor must not widen or vanish)", v, ok)
	}
	if present(g.Prop(n1, "k")) {
		t.Fatal("removed k on n1 still reads")
	}
	if _, ok := g.RelCol("rk"); ok {
		t.Fatal("rk column survived an all-pairs removal")
	}
	// Round trip: the written file must carry no ghost column either.
	var buf bytes.Buffer
	if err := g.WriteRCPG(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := chickpeas.ReadRCPG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Prop(n0, "k").I64(); !ok || v != 7 {
		t.Fatalf("round-tripped k = (%v, %v), want int 7", v, ok)
	}
	if _, ok := got.RelCol("rk"); ok {
		t.Fatal("round-tripped rk column exists")
	}
}
