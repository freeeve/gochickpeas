package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
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

// TestSlotAgrees covers the batch-constant predicate: an empty or single-row
// batch is vacuously constant, equal values agree, a differing value breaks
// it, and out-of-range reads disqualify unless padNull treats them as Null.
func TestSlotAgrees(t *testing.T) {
	row := func(vs ...value.Value) []value.Value { return vs }

	if !slotAgrees(0, nil, false) || !slotAgrees(0, [][]value.Value{row(value.Int(5))}, false) {
		t.Fatal("empty and single-row batches are vacuously constant")
	}
	if !slotAgrees(0, [][]value.Value{row(value.Int(5)), row(value.Int(5))}, false) {
		t.Fatal("equal values agree")
	}
	if slotAgrees(0, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, false) {
		t.Fatal("a differing value breaks constancy")
	}
	// A constant slot alongside a differing one.
	if !slotAgrees(1, [][]value.Value{row(value.Int(5), value.Int(9)), row(value.Int(6), value.Int(9))}, false) {
		t.Fatal("slot 1 is constant while slot 0 differs")
	}
	// Out-of-range with padNull=false disqualifies (multiple rows).
	if slotAgrees(2, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, false) {
		t.Fatal("out-of-range slot must disqualify when padNull is false")
	}
	// Out-of-range with padNull=true reads Null everywhere -> agrees.
	if !slotAgrees(2, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, true) {
		t.Fatal("out-of-range slot reads Null under padNull, so it agrees")
	}
	// Ragged rows: present-then-absent disqualifies without padNull, agrees
	// with padNull when the present value is Null.
	if slotAgrees(1, [][]value.Value{row(value.Int(5), value.Int(9)), row(value.Int(6))}, false) {
		t.Fatal("a row missing the slot disqualifies without padNull")
	}
	if !slotAgrees(1, [][]value.Value{row(value.Int(5), value.Null()), row(value.Int(6))}, true) {
		t.Fatal("a Null present value matches a padded-Null absent one")
	}
}
