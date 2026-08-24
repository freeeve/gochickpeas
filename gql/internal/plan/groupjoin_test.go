package plan

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// labelScan is a one-stage prefix that scans all nodes of a label.
func labelScan(label string) []Stage {
	return []Stage{&MatchStage{Ops: []BindOp{{
		Kind:   OpScan,
		Source: ScanSource{Kind: ScanLabel, Label: label},
		Labels: []string{label},
	}}}}
}

// TestGjOuterBreadth covers the group-join outer-breadth estimate over the
// deterministic scan paths: an empty prefix is one row, a label scan is the
// label cardinality, an all-scan is the node count, and a carried-in
// ScanArg does not multiply the breadth.
func TestGjOuterBreadth(t *testing.T) {
	g := buildFixture(t)
	if b := gjOuterBreadth(nil, g); b != 1 {
		t.Fatalf("empty breadth = %v, want 1", b)
	}
	if b := gjOuterBreadth(labelScan("Person"), g); b != float64(g.LabelCardinality("Person")) {
		t.Fatalf("Person-scan breadth = %v, want %d", b, g.LabelCardinality("Person"))
	}
	all := []Stage{&MatchStage{Ops: []BindOp{{Kind: OpScan, Source: ScanSource{Kind: ScanAll}}}}}
	if b := gjOuterBreadth(all, g); b != float64(g.NodeCount()) {
		t.Fatalf("all-scan breadth = %v, want %d", b, g.NodeCount())
	}
	arg := []Stage{&MatchStage{Ops: []BindOp{{Kind: OpScan, Source: ScanSource{Kind: ScanArg}}}}}
	if b := gjOuterBreadth(arg, g); b != 1 {
		t.Fatalf("carried scan-arg breadth = %v, want 1 (not multiplied)", b)
	}
}

// TestGjOuterBreadthExpand covers the group-join breadth over expand hops: a
// one-hop expand prices the anchor's resolved first-hop degree (the i==1
// branch), a two-hop expand prices the later hop by its label-conditional
// fan-out (the labeled-source branch), and a stage WHERE applies the
// selectivity multiplier.
func TestGjOuterBreadthExpand(t *testing.T) {
	g := buildFixture(t)
	scan := func() BindOp {
		return BindOp{Kind: OpScan, Slot: 0, Source: ScanSource{Kind: ScanLabel, Label: "Person"}, Labels: []string{"Person"}}
	}
	hop := func(from, to int) BindOp {
		return BindOp{Kind: OpExpand, From: from, To: to, Dir: graph.Outgoing, Types: []string{"KNOWS"}, Labels: []string{"Person"}}
	}

	oneHop := []Stage{&MatchStage{Ops: []BindOp{scan(), hop(0, 1)}}}
	if b := gjOuterBreadth(oneHop, g); b <= 0 {
		t.Fatalf("one-hop breadth = %v, want positive", b)
	}

	twoHop := []Stage{&MatchStage{
		Ops:   []BindOp{scan(), hop(0, 1), hop(1, 2)},
		Where: &ast.Binary{Op: ast.OpGt, LHS: &ast.Var{Name: "x"}, RHS: &ast.Lit{Value: ast.IntLit(0)}},
	}}
	if b := gjOuterBreadth(twoHop, g); b <= 0 {
		t.Fatalf("two-hop breadth = %v, want positive", b)
	}
}

// gjBigGraph has L=1100 nodes (clears the 1024 outer-rows floor) and
// Big=2500 nodes, so a scan of L yields a breadth that covers L's own
// population but not Big's.
func gjBigGraph(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(4000, 0)
	for i := 0; i < 1100; i++ {
		if _, err := b.AddNode("L"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2500; i++ {
		if _, err := b.AddNode("Big"); err != nil {
			t.Fatal(err)
		}
	}
	return graphNew(b.Finalize())
}

// TestGjGate covers the economics gate: it declines below the absolute
// outer-rows floor, and for a passing floor it requires the breadth to cover
// (>= half) each correlation variable's population -- the corr label's own,
// the whole node count for an unlabeled corr var -- and passes when every
// corr label is covered or there are none.
func TestGjGate(t *testing.T) {
	// A 40-row Person outer is far below GroupJoinMinOuterRows (1024).
	if gjGate(&gjCandidate{corrLabels: []string{"Person"}}, labelScan("Person"), buildFixture(t)) {
		t.Fatal("a sub-floor outer breadth must not gate")
	}

	big := gjBigGraph(t)
	stages := labelScan("L") // breadth 1100, clears the floor

	// Covers L's own population (1100 >= 0.5*1100).
	if !gjGate(&gjCandidate{corrLabels: []string{"L"}}, stages, big) {
		t.Fatal("an outer covering the corr label must gate")
	}
	// No corr labels -> only the floor applies.
	if !gjGate(&gjCandidate{}, stages, big) {
		t.Fatal("no corr labels -> the floor alone gates")
	}
	// Big has 2500, needs >= 1250; a breadth of 1100 under-covers it.
	if gjGate(&gjCandidate{corrLabels: []string{"Big"}}, stages, big) {
		t.Fatal("an outer under-covering a corr label must not gate")
	}
	// An unlabeled corr var takes the whole node count (3600, needs 1800).
	if gjGate(&gjCandidate{corrLabels: []string{""}}, stages, big) {
		t.Fatal("an unlabeled corr var uses node count; must not gate when under-covered")
	}
}

func TestVarLabelIn(t *testing.T) {
	start := []stageSpec{{pattern: &ast.Pattern{
		Start: ast.NodePat{Var: "x", Labels: []string{"Person", "Actor"}},
	}}}
	if l := varLabelIn(start, "x"); l != "Person" {
		t.Fatalf("start var label = %q, want Person (first label)", l)
	}
	// A hop node's label resolves too.
	hop := []stageSpec{{pattern: &ast.Pattern{
		Start: ast.NodePat{Var: "a"},
		Hops:  []ast.PatternHop{{Node: ast.NodePat{Var: "b", Labels: []string{"Message"}}}},
	}}}
	if l := varLabelIn(hop, "b"); l != "Message" {
		t.Fatalf("hop var label = %q, want Message", l)
	}
	// A var present but unlabeled, and a var absent entirely, both yield "".
	if l := varLabelIn(hop, "a"); l != "" {
		t.Fatalf("unlabeled var = %q, want empty", l)
	}
	if l := varLabelIn(start, "z"); l != "" {
		t.Fatalf("absent var = %q, want empty", l)
	}
	// A nil-pattern spec is skipped, not dereferenced.
	withNil := []stageSpec{{pattern: nil}, start[0]}
	if l := varLabelIn(withNil, "x"); l != "Person" {
		t.Fatalf("nil-pattern spec must be skipped: got %q", l)
	}
}

// TestCondFanout covers the label-conditional fan-out sum: types with a
// conditional statistic add their average degree; unknown types contribute
// nothing; a request with no qualifying type reports the fall-back miss.
func TestCondFanout(t *testing.T) {
	g := buildFixture(t)
	// Message carries one HAS_CREATOR and one HAS_TAG per node, so the
	// conditional fan-out sums both average degrees.
	total, ok := condFanout("Message", []string{"HAS_CREATOR", "HAS_TAG"}, graph.Outgoing, g)
	if !ok || total <= 0 {
		t.Fatalf("Message fan-out = %v,%v, want positive", total, ok)
	}
	// A single known type contributes no more than the pair.
	one, ok := condFanout("Message", []string{"HAS_CREATOR"}, graph.Outgoing, g)
	if !ok || one <= 0 || one > total {
		t.Fatalf("single-type fan-out = %v,%v (pair %v)", one, ok, total)
	}
	// An unknown type on a known label carries a zero conditional statistic,
	// so the mix still resolves (from the known type's degree alone).
	mix, ok := condFanout("Message", []string{"HAS_CREATOR", "NOPE"}, graph.Outgoing, g)
	if !ok || mix != one {
		t.Fatalf("mixed fan-out = %v,%v, want %v", mix, ok, one)
	}
	// An unknown label has no conditional statistic at all -> ok=false, so
	// the caller falls back to the global fan-out.
	if _, ok := condFanout("NopeLabel", []string{"HAS_CREATOR"}, graph.Outgoing, g); ok {
		t.Fatal("unknown label must report ok=false")
	}
	// No types to sum -> ok=false as well.
	if _, ok := condFanout("Message", nil, graph.Outgoing, g); ok {
		t.Fatal("no types must report ok=false")
	}
}

// TestExprVars covers the read-variable collector: bare refs and property
// bases, duplicates preserved.
func TestExprVars(t *testing.T) {
	e := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "foo"}, RHS: &ast.Var{Name: "y"}}
	got := exprVars(e)
	if !slices.Contains(got, "x") || !slices.Contains(got, "y") {
		t.Fatalf("exprVars = %v, want x and y", got)
	}
	// Duplicates are kept (callers dedup).
	dup := &ast.Binary{Op: ast.OpEq, LHS: &ast.Var{Name: "x"}, RHS: &ast.Var{Name: "x"}}
	if got := exprVars(dup); len(got) != 2 {
		t.Fatalf("exprVars kept-duplicates = %v, want two", got)
	}
	if got := exprVars(&ast.Lit{Value: ast.IntLit(1)}); len(got) != 0 {
		t.Fatalf("literal reads no vars: %v", got)
	}
}

// TestPatternVars covers the pattern-variable lister: appearance order,
// anonymous slots skipped.
func TestPatternVars(t *testing.T) {
	p := &ast.Pattern{
		Start: ast.NodePat{Var: "a"},
		Hops: []ast.PatternHop{
			{Rel: ast.RelPat{Var: "r"}, Node: ast.NodePat{Var: "b"}},
			{Rel: ast.RelPat{Var: ""}, Node: ast.NodePat{Var: "c"}}, // anon rel skipped
		},
	}
	if got := patternVars(p); !slices.Equal(got, []string{"a", "r", "b", "c"}) {
		t.Fatalf("patternVars = %v, want [a r b c]", got)
	}
	// A fully anonymous pattern lists nothing.
	if got := patternVars(&ast.Pattern{Start: ast.NodePat{Var: ""}}); len(got) != 0 {
		t.Fatalf("anonymous pattern vars = %v", got)
	}
}

// TestGJAnchorDecline pins the inner-anchor discriminator (task 325,
// from rustychickpeas' losing-regime report): when the standalone inner
// plan anchors on its own correlation variable, both legs walk the same
// relationships and the hash table is pure cost (measured 1.27x slower
// here, 7.6x in the Rust engine on an unfiltered whole-label shape), so
// the rewrite declines. An inner whose plan anchors on a selective seek
// of the non-correlated side stays admitted -- the Q12-class win the
// discriminator must not touch.
func TestGJAnchorDecline(t *testing.T) {
	b := chickpeas.NewBuilder(64, 64)
	// Few Persons, many Messages: the unfiltered inner anchors on the
	// smaller Person side -- the correlation variable.
	var ps []graph.NodeID
	for range 4 {
		id, _ := b.AddNode("Person")
		ps = append(ps, id)
	}
	for i := range 40 {
		m, _ := b.AddNode("Message")
		_ = b.SetProp(m, "lang", []string{"ar", "hu"}[i%2])
		if _, err := b.AddRel(m, ps[i%4], "HAS_CREATOR"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(b.Finalize("lang"))

	hasGJ := func(q string) bool {
		t.Helper()
		p := mustPlan(t, g, q)
		for _, seg := range p.Branches[0] {
			for _, st := range seg.Stages {
				if _, ok := st.(*GroupJoinStage); ok {
					return true
				}
			}
		}
		return false
	}

	defer func(v float64) { GroupJoinMinOuterRows = v }(GroupJoinMinOuterRows)
	GroupJoinMinOuterRows = 0

	losing := "MATCH (p:Person) OPTIONAL MATCH (p)<-[:HAS_CREATOR]-(m:Message) RETURN p, count(m) AS c"
	admitted := "MATCH (p:Person) OPTIONAL MATCH (p)<-[:HAS_CREATOR]-(m:Message {lang: 'ar'}) RETURN p, count(m) AS c"

	// The unfiltered inner anchors on the correlation variable: decline,
	// with the counter as the engagement oracle.
	before := gjAnchorDeclines
	if hasGJ(losing) {
		t.Fatal("corr-anchored inner still group-joins -- the discriminator did not fire")
	}
	if gjAnchorDeclines == before {
		t.Fatal("no decline counted -- the shape dissolved before the discriminator (vacuous)")
	}

	// The disable switch restores the always-admit behavior (the
	// differential leg for exec identity tests).
	DisableGJAnchorDecline = true
	stillGJ := hasGJ(losing)
	DisableGJAnchorDecline = false
	if !stillGJ {
		t.Fatal("disabled discriminator still declines -- switch dead")
	}

	// A selective seek on the non-correlated side anchors the inner away
	// from the correlation key: admitted, counter untouched.
	before = gjAnchorDeclines
	if !hasGJ(admitted) {
		t.Fatal("seek-anchored inner declined -- the Q12-class win would regress")
	}
	if gjAnchorDeclines != before {
		t.Fatal("admitted shape bumped the decline counter")
	}
}

// TestGJRepetitionUsesKeyDomain pins the multi-key repetition estimate
// (task 327, from rustychickpeas 80e59cc): a two-key outer has one row
// per key TUPLE, so breadth divided by any single label's population
// reads as amortizable repetition while breadth over the domain PRODUCT
// correctly reads ~1 -- and an unselective inner must then decline. The
// disable switch doubles as the false-admit demonstration.
func TestGJRepetitionUsesKeyDomain(t *testing.T) {
	b := chickpeas.NewBuilder(64, 64)
	// 4 A x 4 B cross outer (16 rows, one per (a,b) tuple); unselective
	// inner touching both keys.
	var as, bs []graph.NodeID
	for range 4 {
		a, _ := b.AddNode("A")
		as = append(as, a)
		bb, _ := b.AddNode("B")
		bs = append(bs, bb)
	}
	for i := range 16 {
		x, _ := b.AddNode("X")
		if _, err := b.AddRel(as[i%4], x, "R"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddRel(bs[i/4], x, "S"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(b.Finalize())

	hasGJ := func(q string) bool {
		t.Helper()
		p := mustPlan(t, g, q)
		for _, seg := range p.Branches[0] {
			for _, st := range seg.Stages {
				if _, ok := st.(*GroupJoinStage); ok {
					return true
				}
			}
		}
		return false
	}

	defer func(v float64) { GroupJoinMinOuterRows = v }(GroupJoinMinOuterRows)
	GroupJoinMinOuterRows = 0

	q := "MATCH (a:A), (b:B) OPTIONAL MATCH (a)-[:R]->(x)<-[:S]-(b) RETURN a, b, count(x) AS c"
	before := gjAnchorDeclines
	if hasGJ(q) {
		t.Fatal("two-key unselective shape still group-joins -- repetition divided per label, not by the domain")
	}
	if gjAnchorDeclines == before {
		t.Fatal("no decline counted -- the shape dissolved before the gate (vacuous)")
	}
	// The disabled leg still admits, proving the shape reaches the
	// rewrite and only the economics gate refuses it.
	DisableGJAnchorDecline = true
	admitted := hasGJ(q)
	DisableGJAnchorDecline = false
	if !admitted {
		t.Fatal("disabled discriminator still declines -- the decline is not the economics gate's")
	}
}
