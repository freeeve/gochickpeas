package chickpeas

import "testing"

// TestDroppedCrossTypedStagings covers the cross-typed staging diagnostic
// (task 311): the snapshot stores one column (one dtype) per key, so a
// key staged under several value types keeps only the last-finalized
// family's pairs -- the diagnostic reports exactly how many pairs
// vanished, and zero on a well-typed graph. Survival is asserted through
// Prop alongside the count so the number is tied to what a reader sees.
func TestDroppedCrossTypedStagings(t *testing.T) {
	t.Run("well-typed graph reports zero", func(t *testing.T) {
		b := NewBuilder(8, 8)
		n0, _ := b.AddNode("N")
		n1, _ := b.AddNode("N")
		if err := b.SetProp(n0, "age", int64(30)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n1, "name", "x"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddRel(n0, n1, "R"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(0, "w", 1.5); err != nil {
			t.Fatal(err)
		}
		g := b.Finalize()
		if got := g.DroppedCrossTypedStagings(); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
	})

	t.Run("node key staged under two types", func(t *testing.T) {
		b := NewBuilder(32, 0)
		var nodes []NodeID
		for i := range 26 {
			id, err := b.AddNode("Post")
			if err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, id)
			var perr error
			if i < 12 {
				perr = b.SetProp(id, "views", int64(i))
			} else {
				perr = b.SetProp(id, "views", float64(i))
			}
			if perr != nil {
				t.Fatal(perr)
			}
		}
		g := b.Finalize()
		// The float family finalizes after the int family, so the 12
		// int-staged pairs are the ones dropped.
		if got := g.DroppedCrossTypedStagings(); got != 12 {
			t.Fatalf("dropped = %d, want 12", got)
		}
		if _, ok := g.Prop(nodes[0], "views").Value(); ok {
			t.Fatal("int-staged pair survived; the diagnostic should have nothing to report")
		}
		if _, ok := g.Prop(nodes[20], "views").Value(); !ok {
			t.Fatal("float-staged pair vanished; the count no longer matches survival")
		}
	})

	t.Run("three types on one key drop all but the last family", func(t *testing.T) {
		b := NewBuilder(8, 0)
		for _, v := range []any{int64(1), 2.0, "three"} {
			id, err := b.AddNode("N")
			if err != nil {
				t.Fatal(err)
			}
			if err := b.SetProp(id, "k", v); err != nil {
				t.Fatal(err)
			}
		}
		g := b.Finalize()
		if got := g.DroppedCrossTypedStagings(); got != 2 {
			t.Fatalf("dropped = %d, want 2 (int and float lose to the string family)", got)
		}
		if s, ok := g.Prop(NodeID(2), "k").Str(); !ok || s != "three" {
			t.Fatalf("surviving family = %q,%v, want the string staging", s, ok)
		}
	})

	t.Run("rel columns count too", func(t *testing.T) {
		b := NewBuilder(8, 8)
		n0, _ := b.AddNode("N")
		n1, _ := b.AddNode("N")
		for range 2 {
			if _, err := b.AddRel(n0, n1, "R"); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.SetRelPropAt(0, "w", int64(1)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(1, "w", 2.0); err != nil {
			t.Fatal(err)
		}
		g := b.Finalize()
		if got := g.DroppedCrossTypedStagings(); got != 1 {
			t.Fatalf("dropped = %d, want 1 (the int-staged rel pair)", got)
		}
	})
}
