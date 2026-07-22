package plan

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// Planner tests for shortest-path/path-bind lowering, endpoint-collapse
// gating under DISTINCT/aggregation, and property-seek anchor selection.
// Split from plan_test.go (which keeps the shared fixtures/helpers and the
// anchor/quantifier/cost-pass tests).

func TestShortestPathLowering(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[:KNOWS]->{1,6}(b) RETURN pth")
	seg := p.Branches[0][0]
	var sp *SpStage
	for _, s := range seg.Stages {
		if v, ok := s.(*SpStage); ok {
			sp = v
		}
	}
	if sp == nil {
		t.Fatal("no shortest-path stage")
	}
	if sp.All || sp.Max == nil || *sp.Max != 6 {
		t.Fatalf("sp = %+v, want ANY SHORTEST max 6", sp)
	}
	p = mustPlan(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ALL SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN pth")
	for _, s := range p.Branches[0][0].Stages {
		if v, ok := s.(*SpStage); ok && !v.All {
			t.Fatal("ALL SHORTEST must set All")
		}
	}
	planErr(t, g, "MATCH pth = ANY SHORTEST (a)-[:KNOWS]->{1,3}(b) RETURN pth", "bound variable")
}

func TestPathBindAndCallSubquery(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH pth = (a:Person {pid: 1})-[:KNOWS]->(b) RETURN pth")
	ms := firstMatch(t, p)
	if ms.PathBind == nil {
		t.Fatal("path bind spec missing")
	}
	planErr(t, g, "MATCH pth = (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c) RETURN pth", "exactly one relationship hop")

	p = mustPlan(t, g, "MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(q) RETURN q.pid AS qp } RETURN p.pid, qp")
	seg := p.Branches[0][0]
	var cs *CallSubqueryStage
	for _, s := range seg.Stages {
		if v, ok := s.(*CallSubqueryStage); ok {
			cs = v
		}
	}
	if cs == nil || len(cs.ImportSlots) != 1 || len(cs.OutSlots) != 1 {
		t.Fatalf("call subquery = %+v", cs)
	}
	if cs.Sub.Columns[0] != "qp" {
		t.Fatalf("sub columns = %v", cs.Sub.Columns)
	}
}

func TestBindErrors(t *testing.T) {
	g := buildFixture(t)
	planErr(t, g, "MATCH (p:Person) RETURN q", "unknown")
	planErr(t, g, "MATCH (p:Person) WHERE count(p) > 1 RETURN p", "aggregates are not allowed in WHERE")
	planErr(t, g, "MATCH (p:Person) RETURN DISTINCT count(p)", "DISTINCT with aggregates")
	planErr(t, g, "MATCH (p:Person) FOR x IN count(p) RETURN x", "aggregates are not allowed in a FOR list")
	planErr(t, g, "MATCH (p:Person) CALL (zzz) { MATCH (q:Person) RETURN q.pid AS w } RETURN w", "unbound variable")
}

func TestDedupEndpointsUnderDistinct(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (a:Person {pid: 1})-[:KNOWS]->{1,3}(b:Person) RETURN DISTINCT b.pid")
	ms := firstMatch(t, p)
	ve := &ms.Ops[1]
	if !ve.DedupEndpoints {
		t.Fatal("bounded var-expand under DISTINCT should dedup endpoints")
	}
	// With a rel variable in scope the flag must stay off.
	p = mustPlan(t, g, "MATCH (a:Person {pid: 1})-[e:KNOWS]->{1,3}(b:Person) RETURN DISTINCT b.pid, size(e) AS n")
	ms = firstMatch(t, p)
	if ms.Ops[1].DedupEndpoints {
		t.Fatal("a named rel variable must keep per-trail rows")
	}
}

// firstVarExpandDedup returns the DedupEndpoints flag of the first var-expand
// op in the first match stage.
func firstVarExpandDedup(t *testing.T, p *Plan) bool {
	t.Helper()
	ms := firstMatch(t, p)
	for i := range ms.Ops {
		if ms.Ops[i].Kind == OpVarExpand {
			return ms.Ops[i].DedupEndpoints
		}
	}
	t.Fatal("no var-expand op in first match stage")
	return false
}

// TestAggEndpointCollapseGate (092): a bounded var-expand binding no relationship
// collapses per-trail rows to one row per endpoint exactly when the projection
// cannot see a duplicate row -- a plain DISTINCT, or a grouped aggregation whose
// every aggregate is multiplicity-insensitive (min/max, or DISTINCT count/sum/
// avg). It must NOT collapse when any aggregate grows with a duplicate row
// (count(*), non-distinct count/sum/avg, collect, collect(DISTINCT)). Collapsing
// an unsound shape silently returns wrong answers -- invisible to a planner
// differential, since both planners run the collapse -- so the gate is pinned
// here directly.
func TestAggEndpointCollapseGate(t *testing.T) {
	g := buildFixture(t)
	base := "MATCH (p:Person {pid: 0})-[:KNOWS]->{1,2}(f:Person) RETURN "
	for _, ret := range []string{
		"DISTINCT f.pid AS x",
		"min(f.pid) AS m",
		"max(f.pid) AS m",
		"count(DISTINCT f) AS n",
		"sum(DISTINCT f.pid) AS s",
		"avg(DISTINCT f.pid) AS a",
	} {
		if p := mustPlan(t, g, base+ret); !firstVarExpandDedup(t, p) {
			t.Errorf("%q: expected endpoint collapse (projection is duplicate-blind)", ret)
		}
	}
	for _, ret := range []string{
		"f.pid AS x",                   // no DISTINCT, no aggregate
		"count(*) AS n",                // grows per duplicate row
		"count(f) AS n",                // non-distinct count
		"sum(f.pid) AS s",              // non-distinct sum
		"avg(f.pid) AS a",              // non-distinct avg
		"collect(f.pid) AS c",          // order- and multiplicity-bearing
		"collect(DISTINCT f.pid) AS c", // set-equal but order-bearing
	} {
		if p := mustPlan(t, g, base+ret); firstVarExpandDedup(t, p) {
			t.Errorf("%q: must NOT collapse (multiplicity-sensitive projection)", ret)
		}
	}
}

// TestWherePropEqualitySeeksIndex (106): a WHERE-form equality on an indexed
// property must seek the index, exactly like the inline {name: ...} spelling --
// not fall back to a full label scan + post-filter.
func TestWherePropEqualitySeeksIndex(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (tg:Tag) WHERE tg.name = 'tagA' RETURN tg")
	src := firstMatch(t, p).Ops[0].Source
	if src.Kind != ScanProperty || src.Label != "Tag" || src.Key != "name" {
		t.Fatalf("anchor = %+v, want a Tag.name property seek (WHERE-form must seek like inline)", src)
	}
	// The equality stays in the WHERE to finalize (superset-source precedent).
	if firstMatch(t, p).Where == nil {
		t.Fatal("lifted equality must remain as a finalizing filter")
	}
}

// TestWherePropParamSeeksButAbstains (106): a param-valued WHERE equality still
// lowers to a ScanProperty (an index seek at runtime) but keeps the param as its
// value -- no plan-time resolution, so a shared cached plan stays value-blind.
func TestWherePropParamSeeksButAbstains(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (tg:Tag) WHERE tg.name = $n RETURN tg")
	src := firstMatch(t, p).Ops[0].Source
	if src.Kind != ScanProperty || src.Key != "name" {
		t.Fatalf("anchor = %+v, want a Tag.name property seek", src)
	}
	if src.Value.Kind != ast.LitParam && src.Value.Kind != ast.LitNamedParam {
		t.Fatalf("seek value kind = %v, want a param (no plan-time value)", src.Value.Kind)
	}
}

// TestScanSourcePicksMostSelectiveProp (107): with several inline props the
// lowerer must seek the SAME one anchorCard scored -- the smallest posting --
// not the first written. Here the selective prop (email, 1 node) is written
// second behind a common one (country, 10 nodes); the pre-107 code scanned
// country. This regression is invisible to row parity (it returns the right
// rows), which is exactly why 112's plan corpus exists.
func TestScanSourcePicksMostSelectiveProp(t *testing.T) {
	b := chickpeas.NewBuilder(16, 4)
	for i := range 10 {
		n, err := b.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "country", "US"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := b.SetProp(n, "email", "rare@x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := graph.New(b.Finalize("country", "email"))
	p := mustPlan(t, g, "MATCH (n:Person {country: 'US', email: 'rare@x'}) RETURN n")
	src := firstMatch(t, p).Ops[0].Source
	if src.Kind != ScanProperty || src.Key != "email" {
		t.Fatalf("seek key = %q, want email (most selective posting), not the first-written prop", src.Key)
	}
}
