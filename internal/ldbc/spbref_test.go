package ldbc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadSPBRefRows covers the two row shapes of the combined SPB
// parity JSON: bare strings (uris kinds) wrapping into one-cell rows,
// and array rows (kv & co) passing through with python's int/float
// split; and that the hash ignores row order.
func TestLoadSPBRefRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spb.parity.rust.json")
	doc := `{"params":{"word":"football"},"queries":{
		"q1":{"kind":"uris","rows":["http://a","http://b"]},
		"a5":{"kind":"kv","rows":[["k1",2],["k2",3.5]]}}}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	q1, err := LoadSPBRefRows(path, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q1) != 2 || len(q1[0]) != 1 || q1[0][0] != "http://a" {
		t.Fatalf("q1 rows = %v, want wrapped one-cell string rows", q1)
	}

	a5, err := LoadSPBRefRows(path, "a5")
	if err != nil {
		t.Fatal(err)
	}
	if len(a5) != 2 || a5[0][0] != "k1" || a5[0][1] != int64(2) || a5[1][1] != 3.5 {
		t.Fatalf("a5 rows = %v, want [k1 2(int64)] [k2 3.5(float64)]", a5)
	}

	if _, err := LoadSPBRefRows(path, "zz"); err == nil {
		t.Fatal("unknown query should error")
	}

	// Hash equals the same rows hashed in any order.
	h, err := SPBRefHash(path, "q1")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := RowsHash([][]any{{"http://b"}, {"http://a"}})
	if err != nil {
		t.Fatal(err)
	}
	if h != rev {
		t.Fatalf("hash %s != reversed-order hash %s", h, rev)
	}
}
