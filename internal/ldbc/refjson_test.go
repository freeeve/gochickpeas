// Reference JSON loading: the int/float split matching python's, nested
// lists, unsupported cell types, and RefHash agreeing with RowsHash over
// the decoded rows.
package ldbc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRefRowsAndRefHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ref.json")
	// 5 stays int64, 5.0 and 1e3 become float64, nested lists recurse.
	content := `[[5, 5.0, 1e3, "s", true, null, [1, 2.5]]]`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadRefRows(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0]) != 7 {
		t.Fatalf("rows = %v", rows)
	}
	if _, isInt := rows[0][0].(int64); !isInt {
		t.Fatalf("5 decoded as %T, want int64", rows[0][0])
	}
	if _, isFloat := rows[0][1].(float64); !isFloat {
		t.Fatalf("5.0 decoded as %T, want float64", rows[0][1])
	}
	if _, isFloat := rows[0][2].(float64); !isFloat {
		t.Fatalf("1e3 decoded as %T, want float64", rows[0][2])
	}
	nested, isList := rows[0][6].([]any)
	if !isList || len(nested) != 2 {
		t.Fatalf("nested list decoded as %#v", rows[0][6])
	}
	if _, isInt := nested[0].(int64); !isInt {
		t.Fatalf("nested 1 decoded as %T, want int64", nested[0])
	}
	// RefHash == RowsHash over the same rows.
	want, err := RowsHash(rows)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RefHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("RefHash %s != RowsHash %s", got, want)
	}
	// A non-array row is a loud error.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`[42]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRefRows(bad); err == nil {
		t.Fatal("scalar row loaded without error")
	}
	// An object cell is unsupported.
	obj := filepath.Join(dir, "obj.json")
	if err := os.WriteFile(obj, []byte(`[[{"k": 1}]]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRefRows(obj); err == nil {
		t.Fatal("object cell loaded without error")
	}
	if _, err := RefHash(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("missing file hashed without error")
	}
}
