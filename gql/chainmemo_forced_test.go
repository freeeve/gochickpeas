// Chain-memo forced experiment (task 195 second half): the hash-join
// extraction already memoizes builds per external-slot tuple, which IS
// the single-slot suffix memo -- its gates just never admit the
// qualifying stages (they model branch multiply, not key redundancy).
// This experiment force-relaxes the thresholds on real manifest queries
// and ABBA-times default vs forced, row-multiset-checked; the verdict
// decides whether a redundancy-aware admission gate is worth designing.
//
// Gated on GOCHICKPEAS_GQL_MANIFEST; run under the local-cpu lock with
// -max-load.
package gql

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

func TestChainMemoForcedTiming(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("set GOCHICKPEAS_GQL_MANIFEST to run the forced experiment")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	// The census's qualifying queries with non-trivial wall time.
	want := map[string]bool{"Q2": true, "Q5": true, "Q6": true, "Q7": true, "Q10": true, "Q14": true, "Q17": true, "CR1": true, "CR2": true}
	graphs := map[string]*chickpeas.Snapshot{}
	rowKeys := func(g *chickpeas.Snapshot, q string) []string {
		t.Helper()
		res, err := RunUncached(g, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var out []string
		for r := range res.All() {
			var sb strings.Builder
			for _, v := range r.Values() {
				sb.Write(value.AppendKey(nil, v))
				sb.WriteByte('|')
			}
			out = append(out, sb.String())
		}
		slices.Sort(out)
		return out
	}
	median := func(g *chickpeas.Snapshot, q string, reps int) float64 {
		t.Helper()
		var ms []float64
		for range reps {
			start := time.Now()
			res, err := RunUncached(g, q)
			if err != nil {
				t.Fatal(err)
			}
			for range res.All() {
			}
			ms = append(ms, time.Since(start).Seconds()*1000)
		}
		slices.Sort(ms)
		return ms[len(ms)/2]
	}
	for _, row := range rows {
		if row.Blocked() || !want[row.Query] {
			continue
		}
		g, ok := graphs[row.Graph]
		if !ok {
			g, err = chickpeas.ReadRCPGFile(row.Graph)
			if err != nil {
				t.Fatalf("loading %s: %v", row.Graph, err)
			}
			graphs[row.Graph] = g
		}
		base := rowKeys(g, row.GQL)
		dm := median(g, row.GQL, 3)

		mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 256, 2, 1
		qf := row.GQL + " " // defeat the plan cache under the relaxed gates
		forced := rowKeys(g, qf)
		fm := median(g, qf, 3)
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed

		if !slices.Equal(base, forced) {
			t.Errorf("%s: forced extraction changed the row multiset (%d vs %d rows)", row.Query, len(base), len(forced))
			continue
		}
		fmt.Printf("memoforced %-6s default=%9.3fms forced=%9.3fms ratio=%.2fx\n",
			row.Query, dm, fm, fm/dm)
	}
}
