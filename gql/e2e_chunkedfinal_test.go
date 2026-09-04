// Chunked final-level aggregation end-to-end: the candidate-batch path
// against the per-row path, ordered rows identical, with the engagement
// counter proving the batch actually ran (and the floor declining small
// fills).
package gql_test

import (
	"fmt"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/exec"
)

// chunkGraph: two hubs with 100 and 3 typed neighbors -- one fill above
// the chunk floor, one below it.
func chunkGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 128)
	h1, _ := b.AddNode("Hub")
	_ = b.SetProp(h1, "hid", int64(1))
	h2, _ := b.AddNode("Hub")
	_ = b.SetProp(h2, "hid", int64(2))
	for i := 0; i < 100; i++ {
		n, err := b.AddNode("Leaf")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetProp(n, "v", int64(i))
		if _, err := b.AddRel(h1, n, "T"); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if _, err := b.AddRel(h2, n, "T"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize("chunk")
}

func chunkBoth(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	chunked := hjRowKeysOrdered(t, g, q)
	exec.SetDisableChunkedFinal(true)
	defer exec.SetDisableChunkedFinal(false)
	perRow := hjRowKeysOrdered(t, g, q+" ")
	if !slices.Equal(chunked, perRow) {
		t.Fatalf("ordered-row divergence on %s:\nchunked (%d): %v\nper-row (%d): %v",
			q, len(chunked), chunked, len(perRow), perRow)
	}
	return chunked
}

func TestChunkedFinalCountMatchesPerRow(t *testing.T) {
	g := chunkGraph(t)
	before := exec.ChunkedFinalPushes()
	rows := chunkBoth(t, g,
		"MATCH (h:Hub)-[:T]->(l:Leaf) RETURN h.hid AS hid, count(l) AS n ORDER BY hid")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// The 100-candidate fill batches; the 3-candidate fill stays per-row
	// under the floor -- so the counter moves by exactly 100.
	if d := exec.ChunkedFinalPushes() - before; d != 100 {
		t.Fatalf("chunked pushes moved %d, want 100 (hub1 batch only)", d)
	}
	// Value check without the harness encoding.
	if got := intCol(t, g, "MATCH (h:Hub)-[:T]->(l:Leaf) RETURN h.hid AS hid, count(l) AS n ORDER BY hid", "n"); !slices.Equal(got, []int64{100, 3}) {
		t.Fatalf("counts = %v, want [100 3]", got)
	}
}

func TestChunkedFinalMixedAggsAndDistinct(t *testing.T) {
	g := chunkGraph(t)
	before := exec.ChunkedFinalPushes()
	// min/max/sum over a candidate property plus DISTINCT count: the
	// constant-key resolve fires, the bulk-count shortcut declines, the
	// per-candidate fold runs -- rows must still match the per-row path.
	chunkBoth(t, g,
		"MATCH (h:Hub {hid: 1})-[:T]->(l:Leaf) RETURN count(DISTINCT l) AS d, min(l.v) AS lo, max(l.v) AS hi, sum(l.v) AS s")
	if d := exec.ChunkedFinalPushes() - before; d != 100 {
		t.Fatalf("chunked pushes moved %d, want 100", d)
	}
	got := hjRowKeysOrdered(t, g, fmt.Sprintf("MATCH (h:Hub {hid: 1})-[:T]->(l:Leaf) RETURN count(DISTINCT l) AS d, min(l.v) AS lo, max(l.v) AS hi, sum(l.v) AS s%s", ""))
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
}
