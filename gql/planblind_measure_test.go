// Template-blind plan-flip measurement (task 138): for every manifest
// query, compare the value-sighted plan against the plan the cache path
// would EXECUTE for the same values (auto-parameterized template, the
// adaptive anchor chooser bound to the lifted constants), operator trees
// compared value-blind. Reports the flip rate and the tie census -- the
// evidence for whether the ">1 tie stays static" ceiling ever bites on
// the real workload before any fuller choice mechanism is designed.
//
// Gated on GOCHICKPEAS_GQL_MANIFEST (the gql_variants.tsv path); loads
// every manifest graph, so run it under the local-cpu lock.
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

func TestPlanBlindFlipRate(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("set GOCHICKPEAS_GQL_MANIFEST to run the plan-flip measurement")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	graphs := map[string]*chickpeas.Snapshot{}
	type flip struct{ id, sighted, blind string }
	var flips []flip
	planned, tied, multiTied, altChosen := 0, 0, 0, 0
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
		gr := graph.New(g)

		// Value-sighted: the plan the literal text produces.
		qs, err := parseDesugar(row.GQL)
		if err != nil {
			t.Fatalf("%s: parse: %v", row.Query, err)
		}
		sighted, err := plan.Build(qs, gr)
		if err != nil {
			t.Fatalf("%s: sighted plan: %v", row.Query, err)
		}

		// Template-blind: lift the constants, plan the template, then let
		// the adaptive chooser bind this row's values -- exactly the
		// cached executor's path.
		qb, err := parseDesugar(row.GQL)
		if err != nil {
			t.Fatalf("%s: parse: %v", row.Query, err)
		}
		lifted := semantics.AutoParameterize(qb)
		blind, err := plan.Build(qb, gr)
		if err != nil {
			t.Fatalf("%s: blind plan: %v", row.Query, err)
		}
		planned++
		if blind.Ties >= 1 {
			tied++
		}
		if blind.Ties > 1 {
			multiTied++
			t.Logf("%s: %d ties (ceiling: %d stay static)", row.Query, blind.Ties, blind.Ties-1)
		}
		ctx := &eval.Ctx{G: gr, Params: lifted}
		chosen := chooseAdaptivePlan(blind, ctx, gr)
		if chosen != blind {
			altChosen++
		}

		sc := valueBlind(explain.Canonical(sighted, plan.Estimate(sighted, gr)))
		bc := valueBlind(explain.Canonical(chosen, plan.Estimate(chosen, gr)))
		if sc != bc {
			flips = append(flips, flip{row.Query, sc, bc})
		}
	}
	t.Logf("planned=%d flips=%d tied(>=1)=%d multiTied(>1)=%d adaptiveAltChosen=%d",
		planned, len(flips), tied, multiTied, altChosen)
	for _, f := range flips {
		t.Logf("FLIP %s\n--- sighted ---\n%s\n--- blind ---\n%s", f.id, f.sighted, f.blind)
	}
	// The measurement is the log; the assertion only guards the harness
	// (an empty manifest or zero planned queries is a broken run).
	if planned == 0 {
		t.Fatal("no queries planned")
	}
	fmt.Printf("planblind: planned=%d flips=%d tied=%d multiTied=%d altChosen=%d\n",
		planned, len(flips), tied, multiTied, altChosen)
}
