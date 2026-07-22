package chickpeas

import (
	"bytes"
	"fmt"
	"testing"
)

// Generated-workload coverage for the copy-on-write refinalize path: the
// byte-driven edit-script applier, the fuzz parity gate it feeds, and the
// Finalize benchmark. The example-based unit tests live in cow_test.go.

// applyOps replays a byte-driven edit script against a thawed builder. The
// same script against two builders thawed from the same source leaves them in
// identical staging states, which is what lets FuzzRefinalizeMatchesRebuild
// compare the aliasing Finalize against the general one.
func applyOps(b *Builder, script []byte) {
	for i, op := range script {
		node := NodeID(op % 8)
		relIdx := 0
		if len(b.rels) > 0 {
			relIdx = i % len(b.rels)
		}
		switch op % 8 {
		case 0:
			_ = b.UpdateProp(node, "age", int64(op))
		case 1:
			_ = b.SetProp(node, "name", fmt.Sprintf("s%d", op%4))
		case 2:
			_, _ = b.AddRel(node, NodeID((op/8)%8), "KNOWS")
		case 3:
			if len(b.rels) > 0 {
				_ = b.RemoveRel(relIdx)
			}
		case 4:
			_, _ = b.AddNodeWithID(node, "Person")
		case 5:
			b.RemoveNode(node)
		case 6:
			// Rel-property edits keep the CSR clean: the rebuilt rel column
			// must remap through the source's position map.
			_ = b.SetRelPropAt(relIdx, "weight", float64(op)+0.75)
		case 7:
			_, _ = b.RemoveRelPropAt(relIdx, "since")
		}
	}
}

// FuzzRefinalizeMatchesRebuild is the parity gate for the copy-on-write path:
// for any edit script, the snapshot Finalize produces by aliasing clean
// components must be byte-identical to the one it produces by rebuilding
// everything from the same staging state -- and the source snapshot must be
// bit-for-bit unchanged after both.
func FuzzRefinalizeMatchesRebuild(f *testing.F) {
	for _, seed := range [][]byte{
		{}, {0}, {1, 1}, {2}, {3}, {4}, {5}, {0, 2, 5, 3, 1},
		{5, 2, 0}, {2, 2, 2, 3, 3}, {4, 50, 0, 1}, {1, 5, 4, 2, 0, 3},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 64 {
			script = script[:64]
		}
		src := cowSource(t)
		var before bytes.Buffer
		if err := src.WriteRCPG(&before); err != nil {
			t.Fatal(err)
		}

		aliased := NewBuilderFromSnapshot(src)
		applyOps(aliased, script)

		rebuilt := NewBuilderFromSnapshot(src)
		applyOps(rebuilt, script)
		// Drop the source link AFTER the edits: the dirty marks are no-ops
		// without it, and Finalize then rebuilds every component.
		rebuilt.src, rebuilt.srcRelToOutCSR = nil, nil

		var gotAliased, gotRebuilt bytes.Buffer
		if err := aliased.Finalize().WriteRCPG(&gotAliased); err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.Finalize().WriteRCPG(&gotRebuilt); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotAliased.Bytes(), gotRebuilt.Bytes()) {
			t.Fatalf("aliased refinalize diverges from full rebuild (script %v)", script)
		}

		var after bytes.Buffer
		if err := src.WriteRCPG(&after); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Fatalf("source snapshot mutated by its successor (script %v)", script)
		}
	})
}

// benchGraph builds a graph big enough that rebuilding its CSR and columns
// dominates a refinalize.
func benchGraph(b *testing.B, nodes, relsPerNode int) *Snapshot {
	b.Helper()
	bl := NewBuilder(nodes, nodes*relsPerNode)
	for id := range NodeID(nodes) {
		if _, err := bl.AddNodeWithID(id, "Person"); err != nil {
			b.Fatal(err)
		}
		if err := bl.SetProp(id, "age", int64(id%100)); err != nil {
			b.Fatal(err)
		}
		if err := bl.SetProp(id, "name", fmt.Sprintf("n%d", id%1000)); err != nil {
			b.Fatal(err)
		}
	}
	for id := range NodeID(nodes) {
		for k := range relsPerNode {
			dst := NodeID((int(id)*7 + k*13 + 1) % nodes)
			idx, err := bl.AddRel(id, dst, "KNOWS")
			if err != nil {
				b.Fatal(err)
			}
			if err := bl.SetRelPropAt(idx, "weight", float64(k)); err != nil {
				b.Fatal(err)
			}
		}
	}
	return bl.Finalize()
}

// BenchmarkRefinalize measures the Finalize half of the update loop (thaw is
// excluded; it stays O(n + m) until the lazy-thaw follow-up) for a no-edit
// pass, a one-property edit, and a one-rel edit. Each runs both ways: sharing
// the source's clean components, and rebuilding every component from the same
// staging state -- the path a from-scratch builder takes, and the before
// picture for this optimization.
func BenchmarkRefinalize(b *testing.B) {
	src := benchGraph(b, 50_000, 4)
	edits := []struct {
		name string
		edit func(*Builder)
	}{
		{"no_edit", func(*Builder) {}},
		{"one_property", func(bl *Builder) { _ = bl.UpdateProp(7, "age", int64(101)) }},
		{"one_rel", func(bl *Builder) { _, _ = bl.AddRel(1, 2, "KNOWS") }},
	}
	for _, e := range edits {
		for _, share := range []bool{true, false} {
			mode := "aliased"
			if !share {
				mode = "rebuilt"
			}
			b.Run(e.name+"/"+mode, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					bl := NewBuilderFromSnapshot(src)
					e.edit(bl)
					if !share {
						bl.src, bl.srcRelToOutCSR = nil, nil
					}
					b.StartTimer()
					bl.Finalize()
				}
			})
		}
	}
}
