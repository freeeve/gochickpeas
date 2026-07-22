package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// TestUniqPair covers the edge-canonical relationship-uniqueness key: a
// directed hop keys on the edge's (source, target) orientation, while an
// undirected hop keys on the unordered (min, max) pair.
func TestUniqPair(t *testing.T) {
	if a, b := uniqPair(graph.Outgoing, 3, 7); a != 3 || b != 7 {
		t.Fatalf("outgoing = (%d,%d), want (3,7)", a, b)
	}
	if a, b := uniqPair(graph.Incoming, 3, 7); a != 7 || b != 3 {
		t.Fatalf("incoming = (%d,%d), want (7,3)", a, b)
	}
	// Undirected normalizes to (min, max) regardless of traversal order.
	if a, b := uniqPair(graph.Both, 7, 3); a != 3 || b != 7 {
		t.Fatalf("both(7,3) = (%d,%d), want (3,7)", a, b)
	}
	if a, b := uniqPair(graph.Both, 3, 7); a != 3 || b != 7 {
		t.Fatalf("both(3,7) = (%d,%d), want (3,7)", a, b)
	}
}

// TestUniqEnvUsed covers the scope's used-pair check: a live entry matching
// scope+pair is used, a dead (capture-bookkeeping) entry is not, and a
// mismatched scope or pair is not.
func TestUniqEnvUsed(t *testing.T) {
	u := &uniqEnv{stack: []uniqKey{
		{scope: 1, a: 3, b: 7},
		{scope: 1, a: 5, b: 9, dead: true},
	}}
	if !u.used(1, 3, 7) {
		t.Fatal("a live matching pair must be used")
	}
	if u.used(1, 5, 9) {
		t.Fatal("a dead entry must not count as used")
	}
	if u.used(2, 3, 7) {
		t.Fatal("a different scope must not match")
	}
	if u.used(1, 3, 8) {
		t.Fatal("a different pair must not match")
	}
}
