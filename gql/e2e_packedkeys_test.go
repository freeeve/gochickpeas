// Packed-key group storage end-to-end: the packed regime (group keys
// living only as their uint64 pack) against the materialized form,
// ordered rows identical, engagement and demotion pinned by counter --
// including the sign-extension of negative integer keys and the one-way
// demotion when a key stops packing mid-stream.
package gql_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/exec"
)

func packedBoth(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	packed := hjRowKeysOrdered(t, g, q)
	exec.SetDisablePackedKeys(true)
	defer exec.SetDisablePackedKeys(false)
	materialized := hjRowKeysOrdered(t, g, q+" ")
	if !slices.Equal(packed, materialized) {
		t.Fatalf("ordered-row divergence on %s:\npacked (%d): %v\nmaterialized (%d): %v",
			q, len(packed), packed, len(materialized), materialized)
	}
	return packed
}

func TestPackedKeysEntityPair(t *testing.T) {
	g := chunkGraph(t)
	before := exec.AggPackedKeyGroups()
	// Two-entity group key (the Q4 shape): every (hub, leaf) pair is a
	// group, all keys pack, engagement moves by the group count.
	rows := packedBoth(t, g,
		"MATCH (h:Hub)-[:T]->(l:Leaf) RETURN h, l, count(*) AS c ORDER BY h.hid, l.v")
	if len(rows) != 103 {
		t.Fatalf("groups = %d, want 103", len(rows))
	}
	if d := exec.AggPackedKeyGroups() - before; d != 103 {
		t.Fatalf("packed groups moved %d, want 103", d)
	}
}

func TestPackedKeysNegativeIntSignExtension(t *testing.T) {
	g := chunkGraph(t)
	before := exec.AggPackedKeyGroups()
	// Negative 62-bit integers round-trip through the pack's
	// sign-extension at emission.
	rows := packedBoth(t, g,
		"FOR k IN [-5, 3, -5, -9223372036854775, 3] RETURN k, count(*) AS c ORDER BY k")
	if got := intCol(t, g, "FOR k IN [-5, 3, -5, -9223372036854775, 3] RETURN k, count(*) AS c ORDER BY k", "k"); !slices.Equal(got, []int64{-9223372036854775, -5, 3}) {
		t.Fatalf("keys reconstructed wrong: %v", got)
	}
	_ = rows
	if d := exec.AggPackedKeyGroups() - before; d < 3 {
		t.Fatalf("packed groups moved %d, want >= 3", d)
	}
}

func TestPackedKeysDemotionMidStream(t *testing.T) {
	g := chunkGraph(t)
	gBefore := exec.AggPackedKeyGroups()
	dBefore := exec.AggKeyDemotions()
	// Integer keys pack; the string key arriving after packed groups
	// exist forces the one-way demotion -- rows must still match the
	// materialized path exactly.
	q := "FOR k IN [1, 2, 'x', 1, 'x', 3] RETURN k, count(*) AS c ORDER BY c DESC, k ASC"
	packedBoth(t, g, q)
	if d := exec.AggKeyDemotions() - dBefore; d != 1 {
		t.Fatalf("demotions moved %d, want 1", d)
	}
	if d := exec.AggPackedKeyGroups() - gBefore; d < 2 {
		t.Fatalf("packed groups before demotion moved %d, want >= 2", d)
	}
}
