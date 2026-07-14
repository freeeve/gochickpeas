// The auto-parameterization anchor hazard, proven on a real LDBC query.
// Auto-parameterization lifts a query's inline seek constants to param slots
// so one cached plan is shared across values -- but the anchor-orientation
// tie-break then abstains (no plan-time value) and falls back to the
// average-degree pathCost, which cannot see that a SPECIFIC seeked node is a
// hub. On BI Q3 the literal plan anchors on the selective Country end (11
// first-hop rels) while the auto-parameterized cached plan anchors on the
// TagClass hub (899), the exact 42x-class regression the rustychickpeas twin
// measured. This CHARACTERIZES the current (unfixed) divergence on SF1; when
// the runtime-adaptive anchor lands (task 082) the two must CONVERGE on
// Country and this test's assertion flips to demand that.
//
// SF1-gated like the other LDBC validations: set GOCHICKPEAS_SF1_RCPG. The
// synthetic mechanism proof (no data needed) lives in the plan package's
// TestAutoParamAnchorHazard; this proves it bites a real multi-hop query,
// which the average-degree fallback does NOT save despite direction-split
// degree statistics.
package gql

import (
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// biQ3 is the shared LDBC BI Q3 text: a long chain anchored at BOTH ends by a
// concrete property seek (Country{name} and TagClass{name}).
const biQ3 = "MATCH (:Country {name: 'Burma'})<-[:IS_PART_OF]-(:City)<-[:IS_LOCATED_IN]-" +
	"(person:Person)<-[:HAS_MODERATOR]-(forum:Forum)-[:CONTAINER_OF]->" +
	"(post:Post)<-[:REPLY_OF]-{0,}(message:Message)-[:HAS_TAG]->(:Tag)-[:HAS_TYPE]->" +
	"(:TagClass {name: 'MusicalArtist'}) " +
	"RETURN forum.id AS forumId, count(DISTINCT message) AS messageCount " +
	"ORDER BY messageCount DESC, forumId ASC LIMIT 20"

// firstAnchorLabel is the label the plan's first scan anchors on.
func firstAnchorLabel(p *plan.Plan) string {
	for _, seg := range p.Branches[0] {
		for _, st := range seg.Stages {
			if m, ok := st.(*plan.MatchStage); ok {
				for i := range m.Ops {
					if m.Ops[i].Kind == plan.OpScan {
						return m.Ops[i].Source.Label
					}
				}
			}
		}
	}
	return ""
}

// TestAutoParamAnchorHazardOnBIQ3 proves both the hazard and its fix on a
// shipped LDBC query: the static auto-param plan is value-blind (anchors the
// TagClass hub), but task 082's runtime-adaptive anchor builds a Country-
// anchored sibling and the cached executor resolves the bound Burma/
// MusicalArtist params to it (rustychickpeas twin 82a11df / task 085).
func TestAutoParamAnchorHazardOnBIQ3(t *testing.T) {
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		t.Skip("GOCHICKPEAS_SF1_RCPG unset; skipping SF1 anchor-hazard validation")
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	gr := graph.New(g)

	litQ, err := parser.Parse(biQ3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	litPlan, err := plan.Build(litQ, gr)
	if err != nil {
		t.Fatalf("build literal: %v", err)
	}
	litAnchor := firstAnchorLabel(litPlan)

	paramQ, err := parser.Parse(biQ3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lifted := semantics.AutoParameterize(paramQ)
	paramPlan, err := plan.Build(paramQ, gr)
	if err != nil {
		t.Fatalf("build auto-param: %v", err)
	}
	paramAnchor := firstAnchorLabel(paramPlan)

	// The literal plan resolves real first-hop degrees and anchors on the
	// selective Country end.
	if litAnchor != "Country" {
		t.Fatalf("literal anchor = %q, want Country (the selective end)", litAnchor)
	}
	// The STATIC auto-param plan still anchors the TagClass hub -- the planner
	// cannot see the value. That is by design; the fix is runtime-adaptive, not
	// a different static plan.
	if paramAnchor != "TagClass" {
		t.Fatalf("static auto-param anchor = %q, want TagClass (planner is value-blind)", paramAnchor)
	}
	// The fix (task 082): a flipped sibling was built, anchoring the selective
	// Country end...
	if paramPlan.Alt == nil {
		t.Fatal("expected a flipped sibling plan (Plan.Alt) for BI Q3's param-seek tie")
	}
	if alt := firstAnchorLabel(paramPlan.Alt); alt != "Country" {
		t.Fatalf("sibling anchor = %q, want Country (the flipped orientation)", alt)
	}
	// ...and with Burma/MusicalArtist now bound, the cached executor resolves
	// the choice to the Country-anchored plan -- the hazard is closed at exec.
	ctx := &eval.Ctx{G: gr, Params: lifted}
	if chosen := firstAnchorLabel(chooseAdaptivePlan(paramPlan, ctx, gr)); chosen != "Country" {
		t.Fatalf("adaptive choice anchored %q, want Country (Burma resolves to 11 first-hop rels vs TagClass's 899)", chosen)
	}
}
