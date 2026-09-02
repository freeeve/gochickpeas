// Manifest loading: column split, comment/blank skipping, the malformed
// row error, and the blocked-row marker.
package ldbc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.tsv")
	content := "# header comment\n" +
		"\n" +
		"BI\tQ1\tcanonical\t/g.rcpg\tdeadbeef\t-\tMATCH (n) RETURN n\n" +
		"IC\tIC9\tcanonical\t/g.rcpg\tcafe\tsort\tblocked: not yet\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (comment and blank skipped)", len(rows))
	}
	r := rows[0]
	if r.Family != "BI" || r.Query != "Q1" || r.Variant != "canonical" ||
		r.Graph != "/g.rcpg" || r.RefHash != "deadbeef" || r.Norm != "-" ||
		r.GQL != "MATCH (n) RETURN n" {
		t.Fatalf("row 0 = %+v", r)
	}
	if r.Blocked() {
		t.Fatal("plain row reported blocked")
	}
	if !rows[1].Blocked() {
		t.Fatal("blocked: prefix not detected")
	}
	// Wrong column count is a loud error naming the line.
	bad := filepath.Join(dir, "bad.tsv")
	if err := os.WriteFile(bad, []byte("only\tthree\tcols\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(bad); err == nil {
		t.Fatal("3-column row loaded without error")
	}
	if _, err := LoadManifest(filepath.Join(dir, "absent.tsv")); err == nil {
		t.Fatal("missing file loaded without error")
	}
}
