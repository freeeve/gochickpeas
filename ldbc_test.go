// Real-data validation against the Rust engine on LDBC SF1 (gated: skips
// unless GOCHICKPEAS_SF1_RCPG points at the Rust-exported sf1.rcpg). The
// expected values in testdata/ldbc/sf1_expected.json are produced by the
// Rust side (rustychickpeas-ldbc task 256) -- the same lockstep pattern as
// the RCPG conformance corpus. Checks grow as the fixture grows; a fixture
// section that is absent is skipped, so the harness works from day one.

package chickpeas_test

import (
	"os"
	"sync"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// sf1Cache shares one loaded SF1 snapshot across the LDBC tests -- the
// file is half a gigabyte, so each test binary loads it once.
var sf1Cache struct {
	sync.Mutex
	g *chickpeas.Snapshot
}

func loadSF1(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		t.Skip("GOCHICKPEAS_SF1_RCPG unset; skipping LDBC validation (see rustychickpeas-ldbc task 256)")
	}
	sf1Cache.Lock()
	defer sf1Cache.Unlock()
	if sf1Cache.g == nil {
		g, err := chickpeas.ReadRCPGFile(path)
		if err != nil {
			t.Fatalf("loading %s: %v", path, err)
		}
		sf1Cache.g = g
	}
	return sf1Cache.g
}

func loadLDBCExpected(t *testing.T) *ldbc.Expected {
	t.Helper()
	exp, err := ldbc.Load("testdata/ldbc/sf1_expected.json")
	if os.IsNotExist(err) {
		t.Log("no expected-results fixture yet; running smoke checks only")
		return nil
	}
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return exp
}

// TestLDBCStructural cross-checks the loaded SF1 snapshot against the
// whole-graph facts the Rust engine reports for the same file.
func TestLDBCStructural(t *testing.T) {
	g := loadSF1(t)
	// Smoke floor even without a fixture: SF1 is a big, populated graph.
	if g.NodeCount() == 0 || g.RelCount() == 0 || len(g.Labels()) == 0 {
		t.Fatal("SF1 snapshot loaded empty")
	}
	t.Logf("sf1: %d nodes, %d rels, %d labels, %d rel types, id space %d",
		g.NodeCount(), g.RelCount(), len(g.Labels()), len(g.RelTypes()), g.CSRIDSpace())

	exp := loadLDBCExpected(t)
	if exp == nil || exp.Structural == nil {
		return
	}
	s := exp.Structural
	if s.NodeCount != nil && g.NodeCount() != *s.NodeCount {
		t.Errorf("node count: got %d, want %d", g.NodeCount(), *s.NodeCount)
	}
	if s.RelCount != nil && g.RelCount() != *s.RelCount {
		t.Errorf("rel count: got %d, want %d", g.RelCount(), *s.RelCount)
	}
	if s.CSRIDSpace != nil && g.CSRIDSpace() != *s.CSRIDSpace {
		t.Errorf("csr id space: got %d, want %d", g.CSRIDSpace(), *s.CSRIDSpace)
	}
	for _, pair := range s.RelCountByType {
		if got := g.RelTypeCount(pair.Name); got != pair.Count {
			t.Errorf("rel type %s: got %d, want %d", pair.Name, got, pair.Count)
		}
	}
	for _, pair := range s.LabelCardinalities {
		set, ok := g.NodesWithLabel(pair.Name)
		if !ok {
			t.Errorf("label %s missing", pair.Name)
			continue
		}
		if uint64(set.Len()) != pair.Count {
			t.Errorf("label %s: got %d, want %d", pair.Name, set.Len(), pair.Count)
		}
	}
}

// TestLDBCKernels runs each Go kernel on the loaded SF1 snapshot and
// diffs its output against the fixture section the Rust engine produced
// for the same shape (fixture meta.notes documents each one).
func TestLDBCKernels(t *testing.T) {
	g := loadSF1(t)
	exp := loadLDBCExpected(t)
	if exp == nil {
		t.Skip("no expected-results fixture; nothing to cross-check")
	}
	for _, k := range ldbc.Kernels() {
		t.Run(k.Name, func(t *testing.T) {
			want, ok := k.Want(exp)
			if !ok {
				t.Skip("fixture section absent")
			}
			got, err := k.Rows(g)
			if err != nil {
				t.Fatal(err)
			}
			if err := ldbc.DiffRows(got, want); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d rows MATCH", k.Name, len(got))
		})
	}
}
