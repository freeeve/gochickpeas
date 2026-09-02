package ldbc

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// TestCanonCellVMatchesCanonCell locks the value.Value encoder to the boxed
// [][]any encoder cell-for-cell across every rowhash kind and the edge cases
// that exercise the formatting branches (small-int cache boundary, negatives,
// float trimming, empty/unicode/escaped strings, nested lists). If these
// diverge, a migrated kernel would hash differently and break the parity gate.
func TestCanonCellVMatchesCanonCell(t *testing.T) {
	cases := []struct {
		name string
		v    value.Value
		any  any
	}{
		{"null", value.Null(), nil},
		{"true", value.Bool(true), true},
		{"false", value.Bool(false), false},
		{"int0", value.Int(0), int64(0)},
		{"int255", value.Int(255), int64(255)},
		{"int256", value.Int(256), int64(256)},
		{"intNeg", value.Int(-42), int64(-42)},
		{"intBig", value.Int(9007199254740993), int64(9007199254740993)},
		{"float0", value.Float(0), float64(0)},
		{"floatNegZero", value.Float(-0.0), float64(-0.0)},
		{"floatWhole", value.Float(1), float64(1)},
		{"floatTrim", value.Float(1.5), float64(1.5)},
		{"floatLong", value.Float(123.456789), float64(123.456789)},
		{"strEmpty", value.Str(""), ""},
		{"strAscii", value.Str("hello"), "hello"},
		{"strUnicode", value.Str("café – test"), "café – test"},
		{"strEscape", value.Str("a\"b\\c\n\td"), "a\"b\\c\n\td"},
		{"list", value.List([]value.Value{value.Int(1), value.Str("x"), value.Null()}),
			[]any{int64(1), "x", nil}},
		{"nestedList", value.List([]value.Value{value.List([]value.Value{value.Int(2)}), value.Bool(true)}),
			[]any{[]any{int64(2)}, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotV, errV := CanonCellV(c.v)
			gotA, errA := CanonCell(c.any)
			if (errV == nil) != (errA == nil) {
				t.Fatalf("error mismatch: CanonCellV err=%v, CanonCell err=%v", errV, errA)
			}
			if gotV != gotA {
				t.Fatalf("encoding mismatch: CanonCellV=%q CanonCell=%q", gotV, gotA)
			}
		})
	}
}

// TestApplyNormVAndVerifyCellV covers the value-side norm ops (msday
// column fold, round3 recursion into lists, unwrap1, chaining, the
// error cases) and the verify entry: hash-match, mismatch detail, and
// norm-error propagation.
func TestApplyNormVAndVerifyCellV(t *testing.T) {
	day := int64(86_400_000)
	rows := [][]value.Value{
		{value.Int(3*day + 5), value.Float(1.23456), value.List([]value.Value{value.Float(2.71828), value.Int(7)})},
	}
	normed, err := ApplyNormV(rows, "col0:msday, round3")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := normed[0][0].AsInt(); d != 3 {
		t.Fatalf("msday fold = %d, want 3", d)
	}
	if f, _ := normed[0][1].AsFloat(); f != 1.235 {
		t.Fatalf("round3 = %v, want 1.235", f)
	}
	inner, _ := normed[0][2].AsList()
	if f, _ := inner[0].AsFloat(); f != 2.718 {
		t.Fatalf("round3 in list = %v, want 2.718", f)
	}
	if n, _ := inner[1].AsInt(); n != 7 {
		t.Fatalf("int in list changed: %v", inner[1])
	}
	// Identity norms return the rows as-is.
	if same, _ := ApplyNormV(rows, "-"); &same[0] != &rows[0] {
		t.Fatal("identity norm copied rows")
	}
	// unwrap1 lifts a single list cell into the row.
	wrapped := [][]value.Value{{value.List([]value.Value{value.Int(1), value.Int(2)})}}
	un, err := ApplyNormV(wrapped, "unwrap1")
	if err != nil {
		t.Fatal(err)
	}
	if len(un[0]) != 2 {
		t.Fatalf("unwrap1 row width = %d, want 2", len(un[0]))
	}
	for _, bad := range []string{"colX:msday", "nope", "unwrap1"} {
		src := rows
		if bad == "unwrap1" {
			src = [][]value.Value{{value.Int(1), value.Int(2)}} // two cells: needs one
		}
		if _, err := ApplyNormV(src, bad); err == nil {
			t.Fatalf("norm %q accepted", bad)
		}
	}

	// VerifyCellV: build the expected hash via the same machinery, then
	// check match, mismatch detail, and error propagation.
	cells := [][]value.Value{{value.Int(1), value.Str("x")}}
	h, err := RowsHashV(cells)
	if err != nil {
		t.Fatal(err)
	}
	okRow := ManifestRow{RefHash: h, Norm: "-"}
	match, detail, err := VerifyCellV(okRow, cells)
	if err != nil || !match {
		t.Fatalf("match = %v (%s, %v), want true", match, detail, err)
	}
	badRow := ManifestRow{RefHash: "0000000000000000", Norm: "-"}
	match, detail, err = VerifyCellV(badRow, cells)
	if err != nil || match || detail == "" {
		t.Fatalf("mismatch = (%v, %q, %v), want false with detail", match, detail, err)
	}
	if _, _, err := VerifyCellV(ManifestRow{RefHash: h, Norm: "nope"}, cells); err == nil {
		t.Fatal("bad norm verified without error")
	}
}
