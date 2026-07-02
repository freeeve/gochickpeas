package rcpg_test

import (
	"testing"

	"github.com/freeeve/gochickpeas/rcpg"
)

// TestParseLazyMatchesEagerParse checks the lazy path decodes every corpus
// file identically to the eager path, under both full and topology-only
// options.
func TestParseLazyMatchesEagerParse(t *testing.T) {
	for _, f := range loadManifest(t).Files {
		if f.Expect != "ok" {
			continue
		}
		raw := readCorpusFile(t, f.Name)
		for _, tc := range []struct {
			label string
			opts  rcpg.ParseOptions
		}{
			{"full", rcpg.DefaultParseOptions()},
			{"topology_only", rcpg.TopologyOnlyParseOptions()},
		} {
			t.Run(f.Name+"/"+tc.label, func(t *testing.T) {
				eager, err := rcpg.ParseWith(raw, tc.opts)
				if err != nil {
					t.Fatalf("eager: %v", err)
				}
				lazy, err := rcpg.ParseLazy(rcpg.MemoryFetch(raw), tc.opts)
				if err != nil {
					t.Fatalf("lazy: %v", err)
				}
				if !graphsEqual(eager, lazy) {
					t.Fatal("lazy parse differs from eager parse")
				}
			})
		}
	}
}

// TestTopologyOnlyParseSkipsColumns confirms present column sections are not
// materialized when the options skip them.
func TestTopologyOnlyParseSkipsColumns(t *testing.T) {
	raw := readCorpusFile(t, "all_columns.rcpg")
	g, err := rcpg.ParseWith(raw, rcpg.TopologyOnlyParseOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(g.NodeColumns) != 0 || len(g.RelColumns) != 0 {
		t.Fatalf("topology-only parse materialized %d node / %d rel columns",
			len(g.NodeColumns), len(g.RelColumns))
	}
	if g.NRels == 0 || len(g.Atoms) == 0 {
		t.Fatal("topology-only parse lost topology")
	}
}

// TestDirectoryPlanning checks PlanSections/LoadPrefixLen: a topology-only
// plan must exclude the property column ranges, and its prefix must be
// shorter than the file whenever columns exist.
func TestDirectoryPlanning(t *testing.T) {
	raw := readCorpusFile(t, "all_columns.rcpg")
	dir, err := rcpg.ParseDirectory(raw)
	if err != nil {
		t.Fatal(err)
	}
	full := dir.PlanSections(rcpg.DefaultParseOptions())
	topo := dir.PlanSections(rcpg.TopologyOnlyParseOptions())
	if len(full) != 6 || len(topo) != 4 {
		t.Fatalf("plan sizes: full=%d topo=%d, want 6/4", len(full), len(topo))
	}
	for i := 1; i < len(full); i++ {
		if full[i].Start < full[i-1].End {
			t.Fatal("planned ranges not ascending")
		}
	}
	fullLen := dir.LoadPrefixLen(rcpg.DefaultParseOptions())
	topoLen := dir.LoadPrefixLen(rcpg.TopologyOnlyParseOptions())
	if fullLen != uint64(len(raw)) {
		t.Fatalf("full prefix %d, want file length %d", fullLen, len(raw))
	}
	if topoLen >= fullLen {
		t.Fatalf("topology prefix %d not shorter than full %d", topoLen, fullLen)
	}
	// The prefix really is loadable on its own.
	g, err := rcpg.ParseLazy(rcpg.MemoryFetch(raw[:topoLen]), rcpg.TopologyOnlyParseOptions())
	if err != nil {
		t.Fatalf("prefix load: %v", err)
	}
	if g.NRels == 0 {
		t.Fatal("prefix load lost topology")
	}
}

// TestMemoryFetchBounds checks the range guard.
func TestMemoryFetchBounds(t *testing.T) {
	m := rcpg.MemoryFetch(make([]byte, 10))
	if _, err := m.Fetch(4, 6); err != nil {
		t.Fatalf("in-bounds fetch failed: %v", err)
	}
	if _, err := m.Fetch(4, 7); err == nil {
		t.Fatal("past-EOF fetch succeeded")
	}
	if _, err := m.Fetch(^uint64(0), 2); err == nil {
		t.Fatal("overflowing fetch succeeded")
	}
}
