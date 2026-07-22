// Plan-shape tests: parse GQL, plan against a built Snapshot, assert the
// operator shapes (anchor choice, seeks, reorder, splits, lowering).
package plan

import (
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// buildFixture is a small LDBC-shaped graph: many Persons, few Tags (with
// name), Messages linked by HAS_CREATOR/HAS_TAG, KNOWS between persons.
// Cardinalities are deliberately skewed so anchor choices are stable:
// |Person| = 40, |Message| = 20, |Tag| = 4.
func buildFixture(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(80, 200)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var persons, messages, tags []chickpeas.NodeID
	for i := range 40 {
		p, err := b.AddNode("Person")
		must(err)
		must(b.SetProp(p, "pid", int64(i)))
		persons = append(persons, p)
	}
	for i := range 20 {
		m, err := b.AddNode("Message")
		must(err)
		must(b.SetProp(m, "len", int64(i*10)))
		messages = append(messages, m)
	}
	for i := range 4 {
		tg, err := b.AddNode("Tag")
		must(err)
		must(b.SetProp(tg, "name", "tag"+string(rune('A'+i))))
		tags = append(tags, tg)
	}
	for i, m := range messages {
		_, err := b.AddRel(m, persons[i%len(persons)], "HAS_CREATOR")
		must(err)
		_, err = b.AddRel(m, tags[i%len(tags)], "HAS_TAG")
		must(err)
	}
	for i := range persons {
		_, err := b.AddRel(persons[i], persons[(i+1)%len(persons)], "KNOWS")
		must(err)
	}
	return graphNew(b.Finalize())
}

func graphNew(g *chickpeas.Snapshot) graph.Graph { return graph.New(g) }

// mustPlan parses and plans a query.
func mustPlan(t *testing.T, g graph.Graph, src string) *Plan {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	p, err := Build(q, g)
	if err != nil {
		t.Fatalf("plan %q: %v", src, err)
	}
	return p
}

// planErr expects planning to fail with a message containing want.
func planErr(t *testing.T, g graph.Graph, src, want string) {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	_, err = Build(q, g)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("plan %q: err = %v, want contains %q", src, err, want)
	}
}

// firstMatch is the first stage of the only segment as a MatchStage.
func firstMatch(t *testing.T, p *Plan) *MatchStage {
	t.Helper()
	ms, ok := p.Branches[0][0].Stages[0].(*MatchStage)
	if !ok {
		t.Fatalf("first stage is %T, want MatchStage", p.Branches[0][0].Stages[0])
	}
	return ms
}

func TestAnchorPropertySeekBeatsLabelScan(t *testing.T) {
	g := buildFixture(t)
	// The tag property seek (1 node) must anchor over the Message label
	// scan (20 nodes): the pattern reverses.
	p := mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) RETURN m.len")
	ms := firstMatch(t, p)
	if ms.Ops[0].Kind != OpScan || ms.Ops[0].Source.Kind != ScanProperty {
		t.Fatalf("anchor = %+v, want a property seek", ms.Ops[0].Source)
	}
	if ms.Ops[0].Source.Label != "Tag" {
		t.Fatalf("anchor label = %s, want Tag (reversed)", ms.Ops[0].Source.Label)
	}
	if ms.Ops[1].Kind != OpExpand || ms.Ops[1].Dir != graph.Incoming {
		t.Fatalf("hop = %+v, want reversed incoming expand", ms.Ops[1])
	}
}

func TestAnchorSmallerLabelWinsSameTier(t *testing.T) {
	g := buildFixture(t)
	// Both endpoints are plain label scans (tier 1); the cost tie-break
	// picks the smaller cardinality: Tag (4) over Message (20).
	p := mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN m.len")
	ms := firstMatch(t, p)
	if ms.Ops[0].Source.Kind != ScanLabel || ms.Ops[0].Source.Label != "Tag" {
		t.Fatalf("anchor = %+v, want Tag label scan (cost tie-break)", ms.Ops[0].Source)
	}
}

// buildDegreeSkewFixture is a KNOWS graph whose per-node degrees diverge
// sharply from the relation's AVERAGE degree. One hub person (pid 0) knows
// 20 others (out-degree 20); each of those knows only the hub back
// (in-degree 1 apiece). The graph is degree-symmetric in aggregate --
// AvgDegree(KNOWS, out) == AvgDegree(KNOWS, in) -- so a value-independent
// cost probe cannot tell the two orientations apart, while the resolved
// per-node degrees make one orientation 20x better. This is exactly the
// shape that exposes the auto-parameterization anchor hazard.
func buildDegreeSkewFixture(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(32, 64)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var persons []chickpeas.NodeID
	for i := range 21 {
		p, err := b.AddNode("Person")
		must(err)
		must(b.SetProp(p, "pid", int64(i)))
		persons = append(persons, p)
	}
	// Hub (pid 0) knows everyone; everyone knows only the hub back. Sources
	// and targets are the same 21 nodes, so out/in average degree match; but
	// the hub's out-degree is 20 and every spoke's in-degree is 1.
	for i := 1; i <= 20; i++ {
		_, err := b.AddRel(persons[0], persons[i], "KNOWS")
		must(err)
		_, err = b.AddRel(persons[i], persons[0], "KNOWS")
		must(err)
	}
	return graphNew(b.Finalize())
}

// TestAutoParamAnchorHazard confirms (turns code analysis into a measured
// fact) that auto-parameterization can flip the chosen anchor orientation:
// the literal query anchors on the selective endpoint via the resolved
// first-hop degree, but once the seek constants are lifted to params the
// degree probe abstains and the planner falls back to the (tied) average
// degree, picking the 20x-worse orientation. Locks in the hazard task 081
// diagnosed; the fix, if landed, must keep the two forms convergent.
func TestAutoParamAnchorHazard(t *testing.T) {
	g := buildDegreeSkewFixture(t)
	src := "MATCH (a:Person {pid: 0})-[:KNOWS]->(b:Person {pid: 1}) RETURN a.pid, b.pid"

	// Literal form: both endpoints are single-row property seeks (same leaf
	// cardinality), so the tie breaks on the resolved first-hop degree --
	// hub a fans out 20, spoke b fans in 1 -- and the plan reverses to
	// anchor on b, expanding incoming.
	lit := mustPlan(t, g, src)
	litHop := firstMatch(t, lit).Ops[1]
	if litHop.Kind != OpExpand || litHop.Dir != graph.Incoming {
		t.Fatalf("literal hop = %+v, want reversed incoming expand (anchor on selective b)", litHop)
	}

	// Auto-parameterized form: the same query with its seek constants lifted
	// to param slots. The degree probe abstains on params and the tie falls
	// through to the average-degree pathCost, which is symmetric here, so the
	// planner keeps the forward orientation and anchors on the 20x-worse hub.
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	semantics.AutoParameterize(q)
	par, err := Build(q, g)
	if err != nil {
		t.Fatalf("plan parameterized: %v", err)
	}
	parHop := par.Branches[0][0].Stages[0].(*MatchStage).Ops[1]
	if parHop.Kind != OpExpand {
		t.Fatalf("parameterized hop = %+v, want an expand", parHop)
	}

	// The measured hazard: the two forms disagree on orientation. If a future
	// fix makes parameterization value-independent for this decision, this
	// assertion flips and the test should be updated to demand convergence.
	if parHop.Dir == litHop.Dir {
		t.Fatalf("expected auto-param to DIVERGE from literal orientation, both = %v", litHop.Dir)
	}
	if parHop.Dir != graph.Outgoing {
		t.Fatalf("parameterized hop dir = %v, want Outgoing (anchored on hub a)", parHop.Dir)
	}
}

func TestIDSeekRecognition(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (n) WHERE id(n) = 7 RETURN n")
	ms := firstMatch(t, p)
	if ms.Ops[0].Source.Kind != ScanNodeID {
		t.Fatalf("anchor = %+v, want a node-id seek", ms.Ops[0].Source)
	}
	// The WHERE conjunct is kept and re-checked.
	if ms.Where == nil {
		t.Fatal("id-seek WHERE must be kept")
	}
	// Per-row variant via FOR.
	p = mustPlan(t, g, "FOR pid IN [1, 2, 3] MATCH (n) WHERE id(n) = pid RETURN n")
	seg := p.Branches[0][0]
	ms2, ok := seg.Stages[1].(*MatchStage)
	if !ok || ms2.Ops[0].Source.Kind != ScanNodeIDVar {
		t.Fatalf("anchor = %+v, want a per-row id seek", seg.Stages[1])
	}
}

func TestTextMatchRecognition(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (tg:Tag) WHERE tg.name STARTS WITH 'tag' RETURN tg")
	ms := firstMatch(t, p)
	src := ms.Ops[0].Source
	if src.Kind != ScanTextMatch || src.Label != "Tag" || src.Field != "name" || src.Mode != ast.OpStartsWith {
		t.Fatalf("anchor = %+v, want a text-match candidate scan", src)
	}
	if ms.Where == nil {
		t.Fatal("text-match WHERE must be kept for verification")
	}
}

func TestQuantifierLoweringAndMonoPushdown(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (a:Person)-[e:KNOWS]->{1,3}(b:Person) WHERE all(i IN range(0, size(rels(e)) - 2) WHERE rels(e)[i].ts < rels(e)[i+1].ts) RETURN b.pid")
	ms := firstMatch(t, p)
	var ve *BindOp
	for i := range ms.Ops {
		if ms.Ops[i].Kind == OpVarExpand {
			ve = &ms.Ops[i]
		}
	}
	if ve == nil {
		t.Fatal("no var-expand lowered")
	}
	if ve.Min != 1 || ve.Max == nil || *ve.Max != 3 {
		t.Fatalf("bounds = %d..%v, want 1..3", ve.Min, ve.Max)
	}
	if ve.MonoHop == nil || ve.MonoHop.RelKey != "ts" || !ve.MonoHop.Ascending {
		t.Fatalf("mono = %+v, want ascending ts", ve.MonoHop)
	}
	// The consumed conjunct left the WHERE.
	if ms.Where != nil {
		t.Fatalf("mono conjunct must be consumed, WHERE = %v", ms.Where)
	}
}

func TestQuantifierDefaultsAndErrors(t *testing.T) {
	g := buildFixture(t)
	// GQL `*` = {0,}: an unbounded reach (min 0, no max).
	p := mustPlan(t, g, "MATCH (a:Person)-[:KNOWS]->*(b) RETURN b")
	ms := firstMatch(t, p)
	ve := &ms.Ops[1]
	if ve.Kind != OpVarExpand || ve.Min != 0 || ve.Max != nil {
		t.Fatalf("* lowered to %+v, want min 0 unbounded", ve)
	}
	// Empty bounds error.
	planErr(t, g, "MATCH (a:Person)-[:KNOWS]->{3,1}(b) RETURN b", "empty")
	// A rel variable on an unbounded quantifier is rejected.
	planErr(t, g, "MATCH (a:Person)-[e:KNOWS]->*(b) RETURN b", "reachable set")
}

func TestPerHopPredicatePushdown(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (a:Person)-[e:KNOWS]->{1,2}(b:Person) WHERE all(r IN rels(e) WHERE r.w > 5) RETURN b.pid")
	ms := firstMatch(t, p)
	ve := &ms.Ops[1]
	if ve.RelPred == nil || ve.RelPred.Var != "r" {
		t.Fatalf("rel pred = %+v, want lifted all() predicate", ve.RelPred)
	}
	if ms.Where != nil {
		t.Fatal("consumed hop predicate must leave the WHERE")
	}
}

func TestReorderJoinsMostSelectiveFirst(t *testing.T) {
	g := buildFixture(t)
	// Two disconnected-then-joined patterns: the property-seek pattern
	// must be planned first even though written second.
	p := mustPlan(t, g, "MATCH (p:Person) MATCH (tg:Tag {name: 'tagA'}) RETURN p.pid, tg.name")
	seg := p.Branches[0][0]
	ms, ok := seg.Stages[0].(*MatchStage)
	if !ok || ms.Ops[0].Source.Kind != ScanProperty || ms.Ops[0].Source.Label != "Tag" {
		t.Fatalf("first stage anchor = %+v, want the Tag seek first (reorder)", seg.Stages[0])
	}
}

func TestSplitInteriorAnchor(t *testing.T) {
	g := buildFixture(t)
	// The interior Tag seek is strictly more selective than both
	// endpoints: the chain splits into two arms rooted at the tag.
	p := mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'})<-[:HAS_TAG]-(m2:Message) WHERE m.len > m2.len RETURN m.len")
	seg := p.Branches[0][0]
	if len(seg.Stages) != 2 {
		t.Fatalf("stages = %d, want the interior split's two arms", len(seg.Stages))
	}
	arm1 := seg.Stages[0].(*MatchStage)
	if arm1.Ops[0].Source.Kind != ScanProperty || arm1.Ops[0].Source.Label != "Tag" {
		t.Fatalf("arm1 anchor = %+v, want the interior Tag seek", arm1.Ops[0].Source)
	}
	arm2 := seg.Stages[1].(*MatchStage)
	if arm2.Ops[0].Source.Kind != ScanArg {
		t.Fatalf("arm2 anchor = %+v, want the bound tag argument", arm2.Ops[0].Source)
	}
	if arm2.Where == nil {
		t.Fatal("the WHERE must ride the second arm")
	}
}

func TestSplitBoundInteriorFanoutTie(t *testing.T) {
	// A fully-anchored check pattern: both the start (h) and an interior
	// (c) are bound (c through a connected hop, so it is bound by the
	// time the pattern lowers), tying at anchor cost 0 -- the split must
	// re-root at the interior because its expansion fan-out (1) is
	// strictly smaller than the hub endpoint's (20), turning the hub hop
	// into a bound-target check instead of a 20-wide enumeration.
	b := chickpeas.NewBuilder(64, 128)
	h, _ := b.AddNode("Hub")
	if err := b.SetProp(h, "name", "h"); err != nil {
		t.Fatal(err)
	}
	c, _ := b.AddNode("C")
	if err := b.SetProp(c, "name", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(h, c, "HC"); err != nil {
		t.Fatal(err)
	}
	var mids []chickpeas.NodeID
	for i := range 20 {
		m, _ := b.AddNode("M")
		if err := b.SetProp(m, "mid", int64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddRel(h, m, "BIG"); err != nil {
			t.Fatal(err)
		}
		mids = append(mids, m)
	}
	tn, _ := b.AddNode("T")
	if err := b.SetProp(tn, "name", "t0"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(c, mids[0], "SM"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(c, tn, "TG"); err != nil {
		t.Fatal(err)
	}
	g := graphNew(b.Finalize())

	p := mustPlan(t, g, "MATCH (h:Hub {name: 'h'})-[:HC]->(c:C), (h)-[:BIG]->(m:M)<-[:SM]-(c)-[:TG]->(t:T) RETURN m.mid, t.name")
	found := false
	for _, seg := range p.Branches[0] {
		for _, st := range seg.Stages {
			ms, ok := st.(*MatchStage)
			if !ok {
				continue
			}
			for oi := range ms.Ops {
				op := &ms.Ops[oi]
				if op.Kind == OpExpand && len(op.Types) == 1 && op.Types[0] == "BIG" {
					found = true
					if !op.Rebind {
						t.Fatalf("BIG hop = fresh expansion from the hub; want a bound-target check (re-rooted at the bound interior)")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("no BIG hop in the plan")
	}
}

func TestSegmentBoundariesAndPostWhere(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (p:Person) LET twice = p.pid * 2 FILTER twice > 10 RETURN twice ORDER BY twice LIMIT 3")
	// LET and FILTER are star boundaries: three segments.
	segs := p.Branches[0][0*1:]
	_ = segs
	if n := len(p.Branches[0]); n != 3 {
		t.Fatalf("segments = %d, want 3 (LET, FILTER, RETURN)", n)
	}
	filterSeg := p.Branches[0][1]
	if filterSeg.PostWhere == nil {
		t.Fatal("FILTER lowers to a boundary post-WHERE")
	}
	last := p.Branches[0][2]
	if last.Proj.Limit == nil || *last.Proj.Limit != 3 || len(last.Proj.OrderBy) != 1 {
		t.Fatal("ORDER BY/LIMIT ride the final projection")
	}
}

func TestAggregationBinding(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN tg.name, count(m) ORDER BY count(m) DESC")
	proj := &p.Branches[0][0].Proj
	if !proj.Aggregated || len(proj.GroupIdx) != 1 || len(proj.Aggs) != 1 {
		t.Fatalf("proj = %+v, want one group key and one aggregate", proj)
	}
	if proj.Aggs[0].Kind != AggCount || proj.Aggs[0].OutIdx != 1 {
		t.Fatalf("agg = %+v", proj.Aggs[0])
	}
	if proj.Columns[1] != "count(m)" {
		t.Fatalf("derived column name = %q", proj.Columns[1])
	}
	// Nested aggregate: hidden slot + post projection.
	p = mustPlan(t, g, "MATCH (m:Message) RETURN 1.0 * sum(m.len) AS score")
	proj = &p.Branches[0][0].Proj
	if proj.NHidden != 1 || len(proj.Post) != 1 || len(proj.Aggs) != 1 || proj.Aggs[0].OutIdx != 1 {
		t.Fatalf("nested agg proj = %+v", proj)
	}
}

func TestProjectionBeforeAggregateFusion(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "MATCH (m:Message) LET bucket = m.len / 50 RETURN bucket, count(*) NEXT RETURN bucket ORDER BY bucket")
	// Without fusion the LET boundary is its own star segment; LET emits
	// star projections which don't fuse -- assert the aggregate still
	// groups by the computed alias one segment later.
	found := false
	for _, seg := range p.Branches[0] {
		if seg.Proj.Aggregated && len(seg.Proj.GroupIdx) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("no aggregated segment found")
	}
}

func TestUnionColumnMismatch(t *testing.T) {
	g := buildFixture(t)
	planErr(t, g, "MATCH (p:Person) RETURN p.pid AS a UNION MATCH (p:Person) RETURN p.pid AS b",
		"same columns")
}

func TestCallProcStage(t *testing.T) {
	g := buildFixture(t)
	p := mustPlan(t, g, "CALL wcc('KNOWS') YIELD node, component RETURN node, component")
	cs, ok := p.Branches[0][0].Stages[0].(*CallStage)
	if !ok || cs.Proc.Kind != ProcWcc || cs.Proc.RelType != "KNOWS" {
		t.Fatalf("call stage = %+v", p.Branches[0][0].Stages[0])
	}
	if cs.NodeSlot == NoSlot || cs.ValueSlot == NoSlot {
		t.Fatal("both yields bind slots")
	}
	planErr(t, g, "CALL wcc('KNOWS') YIELD node, rank RETURN node", "does not yield")
	planErr(t, g, "CALL nope() YIELD node RETURN node", "unknown procedure")
	planErr(t, g, "CALL algo.pagerank(true, 'x') YIELD node, value RETURN node", "must be a number")
}
