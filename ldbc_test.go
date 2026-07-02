// Real-data validation against the Rust engine on LDBC SF1 (gated: skips
// unless GOCHICKPEAS_SF1_RCPG points at the Rust-exported sf1.rcpg). The
// expected values in testdata/ldbc/sf1_expected.json are produced by the
// Rust side (rustychickpeas-ldbc task 256) -- the same lockstep pattern as
// the RCPG conformance corpus. Checks grow as the fixture grows; a fixture
// section that is absent is skipped, so the harness works from day one.

package chickpeas_test

import (
	"encoding/json"
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

type ldbcExpected struct {
	NodeCount  *uint32           `json:"node_count"`
	RelCount   *uint64           `json:"relationship_count"`
	CSRIDSpace *uint32           `json:"csr_id_space"`
	RelCounts  map[string]uint64 `json:"relationship_count_by_type"`
	LabelCards map[string]int    `json:"label_cardinalities"`
}

func loadSF1(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		t.Skip("GOCHICKPEAS_SF1_RCPG unset; skipping LDBC validation (see rustychickpeas-ldbc task 256)")
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	return g
}

func loadLDBCExpected(t *testing.T) *ldbcExpected {
	t.Helper()
	raw, err := os.ReadFile("testdata/ldbc/sf1_expected.json")
	if err != nil {
		t.Log("no expected-results fixture yet; running smoke checks only")
		return nil
	}
	var exp ldbcExpected
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return &exp
}

// TestLDBCStructural cross-checks the loaded SF1 snapshot against the
// facts the Rust engine reports for the same file.
func TestLDBCStructural(t *testing.T) {
	g := loadSF1(t)
	// Smoke floor even without a fixture: SF1 is a big, populated graph.
	if g.NodeCount() == 0 || g.RelCount() == 0 || len(g.Labels()) == 0 {
		t.Fatal("SF1 snapshot loaded empty")
	}
	t.Logf("sf1: %d nodes, %d rels, %d labels, %d rel types, id space %d",
		g.NodeCount(), g.RelCount(), len(g.Labels()), len(g.RelTypes()), g.CSRIDSpace())

	exp := loadLDBCExpected(t)
	if exp == nil {
		return
	}
	if exp.NodeCount != nil && g.NodeCount() != *exp.NodeCount {
		t.Errorf("node count: got %d, want %d", g.NodeCount(), *exp.NodeCount)
	}
	if exp.RelCount != nil && g.RelCount() != *exp.RelCount {
		t.Errorf("rel count: got %d, want %d", g.RelCount(), *exp.RelCount)
	}
	if exp.CSRIDSpace != nil && g.CSRIDSpace() != *exp.CSRIDSpace {
		t.Errorf("csr id space: got %d, want %d", g.CSRIDSpace(), *exp.CSRIDSpace)
	}
	for relType, want := range exp.RelCounts {
		if got := g.RelTypeCount(relType); got != want {
			t.Errorf("rel type %s: got %d, want %d", relType, got, want)
		}
	}
	for label, want := range exp.LabelCards {
		set, ok := g.NodesWithLabel(label)
		if !ok {
			t.Errorf("label %s missing", label)
			continue
		}
		if set.Len() != want {
			t.Errorf("label %s: got %d, want %d", label, set.Len(), want)
		}
	}
}
