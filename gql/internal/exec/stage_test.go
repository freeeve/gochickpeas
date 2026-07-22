package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

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
