package rcpg_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/rcpg"
)

func addCorpusSeeds(f *testing.F) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".rcpg") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
}

// FuzzParse: Parse must never panic; any failure must be a typed error, and
// any success must survive a write/parse round trip unchanged.
func FuzzParse(f *testing.F) {
	addCorpusSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := rcpg.Parse(b)
		if err != nil {
			if !errors.Is(err, rcpg.ErrCorrupt) && !errors.Is(err, rcpg.ErrUnsupportedVersion) {
				t.Fatalf("untyped parse error: %v", err)
			}
			return
		}
		var buf bytes.Buffer
		if err := rcpg.Write(g, &buf); err != nil {
			t.Fatalf("write of parsed graph failed: %v", err)
		}
		again, err := rcpg.Parse(buf.Bytes())
		if err != nil {
			t.Fatalf("reparse of rewritten graph failed: %v", err)
		}
		if !graphsEqual(g, again) {
			t.Fatal("write/parse round trip changed the graph")
		}
	})
}

// FuzzParseLazy: whenever the eager path accepts an input, the lazy path
// must accept it and decode the identical graph. The reverse does not hold
// (matching the Rust codec): eager bounds-checks every directory entry, but
// lazy never fetches unwanted or unknown sections, so a file corrupt only in
// a section nobody reads passes lazily while failing eagerly.
func FuzzParseLazy(f *testing.F) {
	addCorpusSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		eager, eagerErr := rcpg.Parse(b)
		if eagerErr != nil {
			return
		}
		lazy, lazyErr := rcpg.ParseLazy(rcpg.MemoryFetch(b), rcpg.DefaultParseOptions())
		if lazyErr != nil {
			t.Fatalf("eager parse succeeded but lazy failed: %v", lazyErr)
		}
		if !graphsEqual(eager, lazy) {
			t.Fatal("lazy parse differs from eager parse")
		}
	})
}
