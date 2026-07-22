package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// Coverage for the hash-join build table's per-key chain helpers, exercised
// directly rather than through a full join: mintChain/link build each key's
// row chain in insertion order, and headKey/headVal return the chain head
// (-1 when the key is absent).

// TestHJTableExpandKeyChain covers the expand-keyed (node-id) chains.
func TestHJTableExpandKeyChain(t *testing.T) {
	tbl := &hjTable{rows: make([]hjRow, 16)}
	add := func(key graph.NodeID, idx int32) {
		ci := tbl.byKey.GetOrCreate(uint64(key), tbl.mintChain)
		tbl.link(ci, idx)
	}
	add(7, 0)
	add(7, 3) // a second row for key 7 chains after row 0
	add(9, 1)

	if h := tbl.headKey(7); h != 0 {
		t.Fatalf("headKey(7) = %d, want 0 (first linked)", h)
	}
	if tbl.rows[0].next != 3 {
		t.Fatalf("row 0 next = %d, want 3 (insertion order preserved)", tbl.rows[0].next)
	}
	if h := tbl.headKey(9); h != 1 {
		t.Fatalf("headKey(9) = %d, want 1", h)
	}
	if h := tbl.headKey(123); h != -1 {
		t.Fatalf("headKey(absent) = %d, want -1", h)
	}
}

// TestHJTableValueKeyChain covers the value-keyed (encoded byte key) chains.
func TestHJTableValueKeyChain(t *testing.T) {
	tbl := &hjTable{rows: make([]hjRow, 16)}
	add := func(k string, idx int32) {
		ci := tbl.byVal.GetOrCreate([]byte(k), tbl.mintChain)
		tbl.link(ci, idx)
	}
	add("x", 2)
	add("x", 5) // a second row for key x chains after row 2
	add("y", 4)

	if h := tbl.headVal([]byte("x")); h != 2 {
		t.Fatalf("headVal(x) = %d, want 2 (first linked)", h)
	}
	if tbl.rows[2].next != 5 {
		t.Fatalf("row 2 next = %d, want 5 (insertion order preserved)", tbl.rows[2].next)
	}
	if h := tbl.headVal([]byte("y")); h != 4 {
		t.Fatalf("headVal(y) = %d, want 4", h)
	}
	if h := tbl.headVal([]byte("z")); h != -1 {
		t.Fatalf("headVal(absent) = %d, want -1", h)
	}
}
