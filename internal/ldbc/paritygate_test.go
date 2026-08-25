// The local GQL parity gate: every manifest row's query runs against its
// graph on THIS working tree, result rows verify against the pinned
// reference hashes (rowhash/v1), the PlanCache path re-verifies against
// the same hashes (cached parity), and each query's canonical plan shape
// diffs against the golden. This is the verification battery's parity
// leg, formerly cmd/gqlbench -verify-only -cached-parity -plans-golden;
// the benching/emission half of that driver lives in rustychickpeas-ldbc
// now, but the GATE must run against the working tree, which their
// pinned-engine checkout cannot do.
//
// Gated: skips unless GOCHICKPEAS_GQL_MANIFEST points at
// gql_variants.tsv. Run under the local-cpu lock (loads ~26M rels):
//
//	GOCHICKPEAS_GQL_MANIFEST=.../viz/data/gql_variants.tsv \
//	  go test ./internal/ldbc -run TestGQLParityGate -count=1 -v
//
// After an INTENDED planner change, regenerate the golden in the same
// commit: add GOCHICKPEAS_PLANS_GOLDEN_CAPTURE=1.
package ldbc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

const goldenPath = "testdata/plans_golden.txt"

// gateCells drains a result into zero-box rowhash rows (each row's cells
// copied out of the reused row buffer).
func gateCells(rs *gql.Rows) [][]value.Value {
	var out [][]value.Value
	for row := range rs.All() {
		vals := row.Values()
		cells := make([]value.Value, len(vals))
		copy(cells, vals)
		out = append(out, cells)
	}
	return out
}

func TestGQLParityGate(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("GOCHICKPEAS_GQL_MANIFEST unset; parity gate needs the gql_variants.tsv manifest")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}

	graphs := map[string]*chickpeas.Snapshot{}
	caches := map[string]*gql.PlanCache{}
	var golden []ldbc.GoldenEntry
	match, diff, skip := 0, 0, 0
	var failures []string
	for _, row := range rows {
		g, ok := graphs[row.Graph]
		if !ok {
			t0 := time.Now()
			g, err = chickpeas.ReadRCPGFile(row.Graph)
			if err != nil {
				t.Fatalf("loading %s: %v", row.Graph, err)
			}
			t.Logf("loaded %s in %.1fs: %d nodes, %d rels",
				filepath.Base(row.Graph), time.Since(t0).Seconds(), g.NodeCount(), g.RelCount())
			graphs[row.Graph] = g
		}
		id := row.Family + "/" + row.Query
		if row.Blocked() {
			skip++
			t.Logf("%-16s SKIP  blocked by manifest", id)
			continue
		}
		res, rerr := gql.RunUncached(g, row.GQL)
		if rerr != nil {
			skip++
			t.Logf("%-16s SKIP  %v", id, rerr)
			continue
		}
		cells := gateCells(res)

		// Plan-shape golden, collected before the parity verdict: a
		// DIFFing query's plan drift is usually the review prompt that
		// explains the DIFF.
		canon, cerr := gql.ExplainCanonical(g, row.GQL)
		if cerr != nil {
			t.Fatalf("%s (plans-golden): %v", id, cerr)
		}
		golden = append(golden, ldbc.GoldenEntry{ID: id, Plan: canon})

		ok, detail, verr := ldbc.VerifyCellV(row, cells)
		if verr != nil {
			t.Fatalf("%s: %v", id, verr)
		}
		if !ok {
			diff++
			failures = append(failures, fmt.Sprintf("%s DIFF %s", id, detail))
			t.Logf("%-16s DIFF  %s", id, detail)
			continue
		}

		// Cached-path parity: run twice so the cache-hit replay (not only
		// the first miss) is checked against the same reference hash.
		c, ok2 := caches[row.Graph]
		if !ok2 {
			c = gql.NewPlanCache(1 << 26)
			caches[row.Graph] = c
		}
		cdiff := ""
		for pass := 0; pass < 2 && cdiff == ""; pass++ {
			cres, cerr := c.Run(g, row.GQL)
			if cerr != nil {
				cdiff = "cached run: " + cerr.Error()
				break
			}
			cmatch, cdetail, cerr := ldbc.VerifyCellV(row, gateCells(cres))
			if cerr != nil {
				t.Fatalf("%s (cached parity): %v", id, cerr)
			}
			if !cmatch {
				cdiff = cdetail
			}
		}
		if cdiff != "" {
			diff++
			failures = append(failures, fmt.Sprintf("%s CDIFF %s", id, cdiff))
			t.Logf("%-16s CDIFF %s", id, cdiff)
			continue
		}
		match++
		t.Logf("%-16s MATCH (%d rows)", id, len(cells))
	}
	t.Logf("%d/%d MATCH, %d DIFF, %d SKIP", match, len(rows), diff, skip)
	if len(failures) > 0 {
		t.Fatalf("parity failures:\n  %s", strings.Join(failures, "\n  "))
	}

	if os.Getenv("GOCHICKPEAS_PLANS_GOLDEN_CAPTURE") != "" {
		if err := os.WriteFile(goldenPath, []byte(ldbc.FormatGolden(golden)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("captured %d canonical plan-shapes to %s", len(golden), goldenPath)
		return
	}
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading plan-shape golden (capture with GOCHICKPEAS_PLANS_GOLDEN_CAPTURE=1): %v", err)
	}
	drift := ldbc.DiffGolden(ldbc.ParseGolden(string(data)), golden, false)
	if len(drift) > 0 {
		t.Fatalf("%d plan-shape drift vs golden (review, then regenerate deliberately):\n  %s",
			len(drift), strings.Join(drift, "\n  "))
	}
	t.Logf("plan-shape golden: %d queries unchanged", len(golden))
}
