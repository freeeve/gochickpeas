// The rowhash/v1 self-test vectors from python/cypher/rowhash.py's
// _selftest -- a port must reproduce these byte-for-byte (the vector hash
// 8356f03559b181d9 is the cross-language checkpoint).

package ldbc

import "testing"

func mustCell(t *testing.T, v any) string {
	t.Helper()
	s, err := CanonCell(v)
	if err != nil {
		t.Fatalf("CanonCell(%v): %v", v, err)
	}
	return s
}

func mustHash(t *testing.T, rows [][]any) string {
	t.Helper()
	h, err := RowsHash(rows)
	if err != nil {
		t.Fatalf("RowsHash: %v", err)
	}
	return h
}

func TestCanonCellVectors(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, "1"},
		{false, "0"},
		{float64(3.0), "3"},
		{int64(3), "3"},
		{105.40911, "105.40911"},
		{105.4091096, "105.40911"},
		{-0.0000001, "0"},
		{0.1019379, "0.101938"},
		{"Slavoj_Žižek", `"Slavoj_Žižek"`},
		{"a\"b\\c\nd", `"a\"b\\c\nd"`},
		{[]any{int64(1), "x", nil}, `[1,"x",null]`},
	}
	for _, c := range cases {
		if got := mustCell(t, c.in); got != c.want {
			t.Errorf("CanonCell(%v): got %s, want %s", c.in, got, c.want)
		}
	}
	if _, err := CanonCell(map[string]any{}); err == nil {
		t.Error("CanonCell(map) should be a hard error")
	}
}

func TestRowsHashVectors(t *testing.T) {
	if got := mustHash(t, [][]any{}); got != "e3b0c44298fc1c14" {
		t.Errorf("empty hash: got %s", got)
	}
	a := mustHash(t, [][]any{{int64(2011), false, int64(2)}, {int64(2010), true, int64(0)}})
	b := mustHash(t, [][]any{{int64(2010), true, int64(0)}, {int64(2011), false, int64(2)}})
	if a != b {
		t.Errorf("multiset hash is order-sensitive: %s vs %s", a, b)
	}
	if got := mustHash(t, [][]any{{int64(1), 2.5, "a"}}); got != "8356f03559b181d9" {
		t.Errorf("vector hash: got %s, want 8356f03559b181d9", got)
	}
}

func TestApplyNormVectors(t *testing.T) {
	got, err := ApplyNorm([][]any{{int64(1), int64(86_400_000*5 + 123), "x"}}, "col1:msday")
	if err != nil {
		t.Fatal(err)
	}
	if got[0][1].(int64) != 5 {
		t.Errorf("col1:msday: got %v", got[0][1])
	}

	got, err = ApplyNorm([][]any{{1769669.4200000004, []any{0.12349, int64(7)}}}, "round3")
	if err != nil {
		t.Fatal(err)
	}
	if got[0][0].(float64) != 1769669.42 {
		t.Errorf("round3 cell: got %v", got[0][0])
	}
	inner := got[0][1].([]any)
	if inner[0].(float64) != 0.123 || inner[1].(int64) != 7 {
		t.Errorf("round3 list: got %v", inner)
	}

	got, err = ApplyNorm([][]any{{[]any{int64(4904420543962503192), int64(4912020368333690835)}}}, "unwrap1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0]) != 2 || got[0][0].(int64) != 4904420543962503192 {
		t.Errorf("unwrap1: got %v", got[0])
	}

	if _, err := ApplyNorm([][]any{{int64(1), int64(2)}}, "unwrap1"); err == nil {
		t.Error("unwrap1 on a two-cell row should error")
	}
	if _, err := ApplyNorm(nil, "nonsense"); err == nil {
		t.Error("unknown norm op should error")
	}
}
