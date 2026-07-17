// Timing half of the sighted-vs-cached cost census (task 146): for every
// manifest query whose cached-template plan differs structurally from the
// value-sighted plan (the flip set, recomputed live), ABBA-interleaved
// warm exec timing of the two paths -- sighted (Run: parse + plan + exec)
// against a warm PlanCache L1 hit. Interleaving absorbs drift; the medians
// are only trustworthy from a quiet box, so run under the local-cpu lock
// with -max-load and discard the run if the lock reports contention.
//
// Gated on GOCHICKPEAS_GQL_MANIFEST; loads every manifest graph.
package gql

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

func TestPlanFlipTimeCensus(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("set GOCHICKPEAS_GQL_MANIFEST to run the timing census")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	graphs := map[string]*chickpeas.Snapshot{}
	const reps = 4 // ABBA x reps -> 8 samples per path per query
	measured := 0
	for _, row := range rows {
		if row.Blocked() {
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

		// Flip status, recomputed live (same recipe as the alloc census).
		gr := graph.New(g)
		qs, err := parseDesugar(row.GQL)
		if err != nil {
			t.Fatalf("%s: parse: %v", row.Query, err)
		}
		sightedPlan, err := plan.Build(qs, gr)
		if err != nil {
			t.Fatalf("%s: sighted plan: %v", row.Query, err)
		}
		qb, _ := parseDesugar(row.GQL)
		lifted := semantics.AutoParameterize(qb)
		blindPlan, err := plan.Build(qb, gr)
		if err != nil {
			t.Fatalf("%s: blind plan: %v", row.Query, err)
		}
		chosen := chooseAdaptivePlan(blindPlan, &eval.Ctx{G: gr, Params: lifted}, gr)
		flip := valueBlind(explain.Canonical(sightedPlan, plan.Estimate(sightedPlan, gr))) !=
			valueBlind(explain.Canonical(chosen, plan.Estimate(chosen, gr)))
		if !flip {
			continue
		}

		// Warm both paths (plans, caches, lazy indexes), then ABBA.
		c := NewPlanCache(64 << 20)
		drain := func(run func() error) float64 {
			t.Helper()
			start := time.Now()
			if err := run(); err != nil {
				t.Fatalf("%s: %v", row.Query, err)
			}
			return time.Since(start).Seconds() * 1000
		}
		sighted := func() error { _, err := RunUncached(g, row.GQL); return err }
		cached := func() error { _, err := c.Run(g, row.GQL); return err }
		if err := sighted(); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := cached(); err != nil {
				t.Fatal(err)
			}
		}
		var sMS, cMS []float64
		for range reps {
			sMS = append(sMS, drain(sighted))
			cMS = append(cMS, drain(cached))
			cMS = append(cMS, drain(cached))
			sMS = append(sMS, drain(sighted))
		}
		slices.Sort(sMS)
		slices.Sort(cMS)
		sMed, cMed := sMS[len(sMS)/2], cMS[len(cMS)/2]
		fmt.Printf("timecensus FLIP %-16s sighted=%8.3fms cached=%8.3fms ratio=%.2fx\n",
			row.Family+"/"+row.Query, sMed, cMed, cMed/sMed)
		measured++
	}
	if measured == 0 {
		t.Fatal("no flip queries measured")
	}
}
