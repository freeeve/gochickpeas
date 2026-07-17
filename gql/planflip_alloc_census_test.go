// Allocation half of the sighted-vs-cached cost census (task 146): for
// every manifest query, measure warm-run allocations on the value-sighted
// path (Run: parse + plan + exec per call) against the cache-hit path
// (PlanCache.Run with a warm L1: exec only), flagging the queries whose
// cached plan differs structurally (the flip set). Allocations are the
// half of the census this machine can measure reliably; the ABBA timing
// half needs a quiet box. Result identity between the two paths is the
// -cached-parity gate's job, not this census's.
//
// Gated on GOCHICKPEAS_GQL_MANIFEST; loads every manifest graph, so run
// it under the local-cpu lock.
package gql

import (
	"fmt"
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

func TestPlanFlipAllocCensus(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("set GOCHICKPEAS_GQL_MANIFEST to run the alloc census")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	graphs := map[string]*chickpeas.Snapshot{}
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

		// Flip status, recomputed live exactly as TestPlanBlindFlipRate
		// does (the 138 list predates several planner-visible changes).
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

		// Sighted path: full Run per call, one warm run measured.
		if _, err := RunUncached(g, row.GQL); err != nil {
			t.Fatalf("%s: sighted run: %v", row.Query, err)
		}
		sa, _, err := ldbc.MeasureAllocs(func() error { _, err := RunUncached(g, row.GQL); return err })
		if err != nil {
			t.Fatalf("%s: sighted measure: %v", row.Query, err)
		}
		// Cached path: warm the L1 with two runs, measure the hit.
		c := NewPlanCache(64 << 20)
		for range 2 {
			if _, err := c.Run(g, row.GQL); err != nil {
				t.Fatalf("%s: cached run: %v", row.Query, err)
			}
		}
		ca, _, err := ldbc.MeasureAllocs(func() error { _, err := c.Run(g, row.GQL); return err })
		if err != nil {
			t.Fatalf("%s: cached measure: %v", row.Query, err)
		}
		mark := " "
		if flip {
			mark = "FLIP"
		}
		fmt.Printf("alloccensus %-4s %-16s sighted=%-8d cached=%-8d saved=%d\n",
			mark, row.Family+"/"+row.Query, sa, ca, int64(sa)-int64(ca))
		measured++
	}
	if measured == 0 {
		t.Fatal("no queries measured")
	}
}
