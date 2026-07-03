// DiffGraphs layer-by-layer tests: each perturbation of a small SPB-style
// graph must be caught by exactly the cheapest layer that can see it, and
// a shuffled reload of the same document must MATCH (the diff keys off
// the external id, never the load-order-dependent dense NodeID).

package ldbc

import (
	"strings"
	"testing"
)

const diffDoc = `
<http://ex/a> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Work> .
<http://ex/a> <http://ex/name> "alpha" .
<http://ex/a> <http://ex/rank> "1"^^<http://www.w3.org/2001/XMLSchema#integer> .
<http://ex/a> <http://ex/knows> <http://ex/b> .
<http://ex/b> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Work> .
<http://ex/b> <http://ex/name> "beta" .
<http://ex/b> <http://ex/likes> <http://ex/c> .
<http://ex/c> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Place> .
<http://ex/c> <http://ex/name> "gamma" .
`

// diffAgainst loads both documents and diffs the second against the
// first with Full on (the loadbench nt configuration).
func diffAgainst(refDoc, testDoc string, opts DiffOpts) (bool, string) {
	ref, _ := LoadSPBString(refDoc)
	test, _ := LoadSPBString(testDoc)
	if opts.IDProp == "" {
		opts.IDProp = "uri"
	}
	return DiffGraphs(ref, test, opts)
}

func TestDiffGraphsMatchesShuffledReload(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(diffDoc), "\n")
	shuffled := make([]string, 0, len(lines))
	for i := range lines {
		shuffled = append(shuffled, lines[len(lines)-1-i])
	}
	ok, detail := diffAgainst(diffDoc, strings.Join(shuffled, "\n"), DiffOpts{Full: true})
	if !ok || detail != "MATCH" {
		t.Fatalf("shuffled reload: ok=%v detail=%q, want MATCH", ok, detail)
	}
}

func TestDiffGraphsCatchesNodeCount(t *testing.T) {
	extra := diffDoc + `<http://ex/d> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Place> .
`
	ok, detail := diffAgainst(diffDoc, extra, DiffOpts{})
	if ok || !strings.HasPrefix(detail, "node_count: ") {
		t.Fatalf("ok=%v detail=%q, want node_count layer", ok, detail)
	}
}

func TestDiffGraphsCatchesLabelCount(t *testing.T) {
	moved := strings.Replace(diffDoc,
		"<http://ex/c> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Place> .",
		"<http://ex/c> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex/Work> .", 1)
	ok, detail := diffAgainst(diffDoc, moved, DiffOpts{})
	if ok || !strings.HasPrefix(detail, "label ") {
		t.Fatalf("ok=%v detail=%q, want label layer", ok, detail)
	}
}

func TestDiffGraphsCatchesRelTypeCount(t *testing.T) {
	retyped := strings.Replace(diffDoc, "<http://ex/likes>", "<http://ex/knows>", 1)
	ok, detail := diffAgainst(diffDoc, retyped, DiffOpts{})
	if ok || !strings.HasPrefix(detail, "rel ") {
		t.Fatalf("ok=%v detail=%q, want rel layer", ok, detail)
	}
}

func TestDiffGraphsCatchesPropValueCorruption(t *testing.T) {
	corrupted := strings.Replace(diffDoc, `"beta"`, `"BETA"`, 1)
	ok, detail := diffAgainst(diffDoc, corrupted, DiffOpts{})
	if ok || !strings.HasPrefix(detail, "props Work: ") {
		t.Fatalf("ok=%v detail=%q, want props layer", ok, detail)
	}
}

func TestDiffGraphsExplicitNodePropsSkipsUnlistedKeys(t *testing.T) {
	corrupted := strings.Replace(diffDoc, `"beta"`, `"BETA"`, 1)
	blind := DiffOpts{NodeProps: map[string][]string{"Work": {"rank"}}}
	if ok, detail := diffAgainst(diffDoc, corrupted, blind); !ok {
		t.Fatalf("detail=%q: rank-only sampling must not see the name corruption", detail)
	}
	listed := DiffOpts{NodeProps: map[string][]string{"Work": {"name"}}}
	if ok, detail := diffAgainst(diffDoc, corrupted, listed); ok || !strings.HasPrefix(detail, "props Work: ") {
		t.Fatalf("ok=%v detail=%q, want props layer", ok, detail)
	}
}

func TestDiffGraphsFullCatchesIDSwapPastSampleWindow(t *testing.T) {
	// Same counts, labels, rels, and props; only the SECOND Work node's
	// external id changes. Node a sorts first under both ids, so a
	// sample of 1 passes every earlier layer and only the full id-set
	// layer can see the swap.
	swapped := strings.ReplaceAll(diffDoc, "<http://ex/b>", "<http://ex/z>")
	opts := DiffOpts{Sample: 1, Full: true}
	ok, detail := diffAgainst(diffDoc, swapped, opts)
	if ok || !strings.HasPrefix(detail, "id-set Work: ") {
		t.Fatalf("ok=%v detail=%q, want id-set layer", ok, detail)
	}
	if !strings.Contains(detail, "1 ref-only, 1 test-only") {
		t.Fatalf("detail=%q, want 1 ref-only, 1 test-only", detail)
	}
}
