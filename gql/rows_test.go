// Result-surface tests: Row lookup by name and position, Rows streaming via
// Next/NextBatch/All.
package gql

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

func fixtureRows() *Rows {
	return newRows([]string{"name", "age"}, [][]value.Value{
		{value.Str("Alice"), value.Int(30)},
		{value.Str("Bob"), value.Int(40)},
		{value.Str("Carol"), value.Null()},
	})
}

func TestRowGetByNameAndPosition(t *testing.T) {
	rs := fixtureRows()
	r, ok := rs.Next()
	if !ok {
		t.Fatal("first row")
	}
	if v, ok := r.Get("name"); !ok || !value.Equal(v, value.Str("Alice")) {
		t.Fatalf("name = %v, %v", v, ok)
	}
	if v, ok := r.Get("age"); !ok || !value.Equal(v, value.Int(30)) {
		t.Fatalf("age = %v, %v", v, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unknown column is not ok")
	}
	if v, ok := r.GetAt(1); !ok || !value.Equal(v, value.Int(30)) {
		t.Fatalf("GetAt(1) = %v, %v", v, ok)
	}
	if _, ok := r.GetAt(2); ok {
		t.Fatal("out of range is not ok")
	}
	if _, ok := r.GetAt(-1); ok {
		t.Fatal("negative index is not ok")
	}
	if len(r.Values()) != 2 || len(r.Columns()) != 2 {
		t.Fatal("Values/Columns lengths")
	}
}

func TestRowsStreaming(t *testing.T) {
	rs := fixtureRows()
	if got := rs.Columns(); len(got) != 2 || got[0] != "name" {
		t.Fatalf("columns = %v", got)
	}
	batch := rs.NextBatch(2)
	if len(batch) != 2 {
		t.Fatalf("batch = %d rows", len(batch))
	}
	// A batch larger than the remainder returns the remainder.
	batch = rs.NextBatch(10)
	if len(batch) != 1 {
		t.Fatalf("tail batch = %d rows", len(batch))
	}
	if v, _ := batch[0].Get("age"); !v.IsNull() {
		t.Fatal("carol's age is null")
	}
	if _, ok := rs.Next(); ok {
		t.Fatal("exhausted")
	}
}

func TestRowsAll(t *testing.T) {
	rs := fixtureRows()
	names := []string{}
	for r := range rs.All() {
		v, _ := r.Get("name")
		s, _ := v.AsStr()
		names = append(names, s)
	}
	if len(names) != 3 || names[0] != "Alice" || names[2] != "Carol" {
		t.Fatalf("names = %v", names)
	}
	// All on an exhausted result yields nothing; early break stops cleanly.
	rs = fixtureRows()
	count := 0
	for range rs.All() {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("early break count = %d", count)
	}
}

func TestErrorSentinelsAreDistinct(t *testing.T) {
	errs := []error{ErrParse, ErrBind, ErrPlan, ErrEval}
	for i, a := range errs {
		for j, b := range errs {
			if (i == j) != (a == b) {
				t.Fatal("sentinels are distinct identities")
			}
		}
	}
}
