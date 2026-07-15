// Runtime-adaptive anchor for auto-parameterized both-ends-param-seek ties
// (task 082). The planner cannot see which end a param resolves to, so it
// builds a primary plan and a flipped sibling (Plan.Alt) and the cached
// executor picks per execution by the anchors' real bound-param degrees. The
// decisive property -- what makes this "adaptive" and not "bake the value into
// the cache key" -- is that ONE cached template chooses OPPOSITE anchors for
// two different parameter values.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// adaptiveFixture builds a graph with degree-symmetric labels (|A| == |B|, so
// the anchor tie survives to the degree probe) but per-node degrees that make
// the cheaper anchor DEPEND ON THE VALUE: a1 fans out 1000 / b1 fans in 1
// (so a-seek 'a1' should defer to b), while a2 fans out 1 / b2 fans in 1000
// (so b-seek 'b2' should defer to a).
func adaptiveFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4096, 8192)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mk := func(label, key, val string) chickpeas.NodeID {
		n, err := b.AddNode(label)
		must(err)
		must(b.SetProp(n, key, val))
		return n
	}
	a1 := mk("A", "k", "a1")
	a2 := mk("A", "k", "a2")
	b1 := mk("B", "k", "b1")
	b2 := mk("B", "k", "b2")
	// a1 -R-> 1000 junk B; a2 -R-> 1 junk B.
	for range 1000 {
		jb, err := b.AddNode("B")
		must(err)
		_, err = b.AddRel(a1, jb, "R")
		must(err)
	}
	jb, err := b.AddNode("B")
	must(err)
	_, err = b.AddRel(a2, jb, "R")
	must(err)
	// 1 junk A -R-> b1; 1000 junk A -R-> b2.
	ja, err := b.AddNode("A")
	must(err)
	_, err = b.AddRel(ja, b1, "R")
	must(err)
	for range 1000 {
		ja2, err := b.AddNode("A")
		must(err)
		_, err = b.AddRel(ja2, b2, "R")
		must(err)
	}
	// |A| = 2 + 1 + 1000 = 1003; |B| = 2 + 1000 + 1 = 1003 -> label cards tie.
	return b.Finalize("adaptive")
}

// chosenAnchorLabel is the label the (adaptively chosen) plan seeds from.
func chosenAnchorLabel(t *testing.T, p *plan.Plan, gr graph.Graph, params []value.Value) string {
	t.Helper()
	ctx := &eval.Ctx{G: gr, Params: params}
	chosen := chooseAdaptivePlan(p, ctx, gr)
	ms := firstMatchStage(chosen)
	if ms == nil || len(ms.Ops) == 0 {
		t.Fatal("no match stage in chosen plan")
	}
	return ms.Ops[0].Source.Label
}

func TestAdaptiveAnchorPicksPerValue(t *testing.T) {
	snap := adaptiveFixture(t)
	gr := graph.New(snap)

	// One template: both ends param seeks after auto-parameterization.
	q, err := parser.Parse("MATCH (a:A {k: 'a1'})-[:R]->(b:B {k: 'b1'}) RETURN a.k, b.k")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	semantics.AutoParameterize(q) // lifts 'a1','b1' to param slots 0,1
	p, err := plan.Build(q, gr)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if p.Alt == nil {
		t.Fatal("expected a flipped sibling plan (Plan.Alt) for the param-seek tie")
	}

	// Value set 1: a='a1' (fan-out 1000), b='b1' (fan-in 1) -> seed from B.
	if got := chosenAnchorLabel(t, p, gr, []value.Value{value.Str("a1"), value.Str("b1")}); got != "B" {
		t.Fatalf("(a1,b1): chose anchor %q, want B (b1 is the selective end)", got)
	}
	// Value set 2: a='a2' (fan-out 1), b='b2' (fan-in 1000) -> seed from A.
	// SAME cached template, OPPOSITE anchor -- the adaptive property.
	if got := chosenAnchorLabel(t, p, gr, []value.Value{value.Str("a2"), value.Str("b2")}); got != "A" {
		t.Fatalf("(a2,b2): chose anchor %q, want A (a2 is the selective end)", got)
	}
}

// TestAdaptiveAnchorViaPrepared pins the Prepared.Execute wiring: the
// prepared-statement path is the canonical parameter-sniffing shape (one
// plan, many value sets), and it used to build the sibling and ignore it.
// Rows are identical under either plan, so the assertion is the chooser's
// fired counter: an Execute whose values invert the degree asymmetry must
// consult and pick the sibling.
func TestAdaptiveAnchorViaPrepared(t *testing.T) {
	snap := adaptiveFixture(t)
	pr, err := Prepare(snap, "MATCH (a:A {k: 'a1'})-[:R]->(b:B {k: 'b1'}) RETURN a.k AS ak, b.k AS bk")
	if err != nil {
		t.Fatal(err)
	}
	// The lifted literals ('a1','b1') make B the selective end: the
	// primary plan was built value-blind, so ONE of the two value sets
	// must flip to the sibling. Run both; the counter must move.
	before := adaptiveAltPicked
	if _, err := pr.Execute(snap, nil); err != nil {
		t.Fatal(err)
	}
	prep2, err := Prepare(snap, "MATCH (a:A {k: 'a2'})-[:R]->(b:B {k: 'b2'}) RETURN a.k AS ak, b.k AS bk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prep2.Execute(snap, nil); err != nil {
		t.Fatal(err)
	}
	if adaptiveAltPicked == before {
		t.Fatal("Prepared.Execute never consulted the adaptive sibling (wiring gap): one of the two inverted value sets must pick it")
	}
}

// TestAdaptiveAnchorTwoTies pins the >=1-tie sibling: a query with TWO
// param-seek patterns used to drop to the static fallback entirely (more
// value-blind decisions, less help). Now the first tie gets its flipped
// sibling; the second stays static by design.
func TestAdaptiveAnchorTwoTies(t *testing.T) {
	snap := adaptiveFixture(t)
	gr := graph.New(snap)
	q, err := parser.Parse("MATCH (a:A {k: 'a1'})-[:R]->(b:B {k: 'b1'}) MATCH (c:A {k: 'a2'})-[:R]->(d:B {k: 'b2'}) RETURN a.k, c.k")
	if err != nil {
		t.Fatal(err)
	}
	semantics.AutoParameterize(q)
	p, err := plan.Build(q, gr)
	if err != nil {
		t.Fatal(err)
	}
	if p.Alt == nil {
		t.Fatal("two anchor ties must still build the first tie's sibling, not drop to the static fallback")
	}
}
