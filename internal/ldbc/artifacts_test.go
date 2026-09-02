// Artifact helpers: the allocation-delta measurement and append-only
// JSONL emission.
package ldbc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeasureAllocs(t *testing.T) {
	var sink [][]byte
	n, b, err := MeasureAllocs(func() error {
		for i := 0; i < 100; i++ {
			sink = append(sink, make([]byte, 1024))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 100 || b < 100*1024 {
		t.Fatalf("measured %d allocs / %d bytes, want at least the 100 KiB the closure made", n, b)
	}
	_ = sink
	wantErr := errors.New("boom")
	if _, _, err := MeasureAllocs(func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("error not propagated: %v", err)
	}
}

func TestAppendJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.jsonl") // parent must be created
	type rec struct{ N int }
	for i := 1; i <= 2; i++ {
		f, enc, err := AppendJSONL(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(rec{N: i}); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("appended runs produced %d lines, want 2 (append-only)", len(lines))
	}
	var r rec
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil || r.N != 2 {
		t.Fatalf("second line = %q (%v)", lines[1], err)
	}
}
