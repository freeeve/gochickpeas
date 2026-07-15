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
