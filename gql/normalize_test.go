// NORMALIZE / IS [NOT] NORMALIZED surface tests (escape-only literals --
// combining marks do not survive editors): the four forms through both
// the function and the predicate, the bare-keyword and quoted form
// arguments, null propagation, and the unknown-form null.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

func normFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	n, err := b.AddNode("S")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(n, "decomposed", "cafe\u0301"); err != nil {
		t.Fatal(err)
	}
	return b.Finalize("norm")
}

// oneRow runs q and returns its single row's values.
func oneRow(t *testing.T, g *chickpeas.Snapshot, q string) []value.Value {
	t.Helper()
	rows, err := Run(g, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []value.Value
	n := 0
	for r := range rows.All() {
		out = append(out[:0], r.Values()...)
		n++
	}
	if n != 1 {
		t.Fatalf("%s: %d rows, want 1", q, n)
	}
	return out
}

func TestNormalizeFunction(t *testing.T) {
	g := normFixture(t)
	// Default form is NFC: the decomposed property composes.
	vals := oneRow(t, g, "MATCH (s:S) RETURN normalize(s.decomposed) AS x, normalize(s.decomposed, 'NFD') AS d, normalize(s.decomposed, NFKC) AS k")
	if s, _ := vals[0].AsStr(); s != "caf\u00e9" {
		t.Fatalf("normalize() = %q, want composed", s)
	}
	if s, _ := vals[1].AsStr(); s != "cafe\u0301" {
		t.Fatalf("normalize(NFD) = %q, want decomposed unchanged", s)
	}
	if s, _ := vals[2].AsStr(); s != "caf\u00e9" {
		t.Fatalf("normalize(NFKC keyword) = %q, want composed", s)
	}
	// Null propagation and the unknown-form null.
	vals = oneRow(t, g, "MATCH (s:S) RETURN normalize(s.missing) AS a, normalize(s.decomposed, 'NFX') AS b, normalize(123) AS c")
	for i, v := range vals {
		if !v.IsNull() {
			t.Fatalf("col %d = %v, want null", i, v)
		}
	}
}

func TestIsNormalizedPredicate(t *testing.T) {
	g := normFixture(t)
	vals := oneRow(t, g, "MATCH (s:S) RETURN s.decomposed IS NORMALIZED AS c, s.decomposed IS NFD NORMALIZED AS d, s.decomposed IS NOT NORMALIZED AS n")
	if b, _ := vals[0].AsBool(); b {
		t.Fatal("decomposed string reported NFC-normalized")
	}
	if b, _ := vals[1].AsBool(); !b {
		t.Fatal("decomposed string not reported NFD-normalized")
	}
	if b, _ := vals[2].AsBool(); !b {
		t.Fatal("IS NOT NORMALIZED did not negate")
	}
	// Null input: the predicate is null (unknown), so a WHERE drops the row.
	rows, err := Run(g, "MATCH (s:S) WHERE s.missing IS NORMALIZED RETURN s")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range rows.All() {
		n++
	}
	if n != 0 {
		t.Fatalf("null-input predicate kept %d rows, want 0", n)
	}
}
