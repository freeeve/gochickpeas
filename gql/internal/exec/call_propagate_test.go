package exec

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// TestCallPropagateRows covers the algo.propagate adapter: it must pair each
// seed with its value, forward the value-property and traversal options, and
// return one row per reached node. Fixture: 0 -R(w=5)-> 1 -R(w=7)-> 2, node
// 3 isolated.
func TestCallPropagateRows(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("N")
	n1, _ := b.AddNode("N")
	n2, _ := b.AddNode("N")
	_, _ = b.AddNode("N") // node 3: isolated
	i0, err := b.AddRel(n0, n1, "R")
	must(err)
	must(b.SetRelPropAt(i0, "w", 5.0))
	i1, err := b.AddRel(n1, n2, "R")
	must(err)
	must(b.SetRelPropAt(i1, "w", 7.0))
	g := b.Finalize("prop")

	byNode := func(res []chickpeas.PropagateResult) map[chickpeas.NodeID]chickpeas.PropagateResult {
		m := map[chickpeas.NodeID]chickpeas.PropagateResult{}
		for _, r := range res {
			m[r.Node] = r
		}
		return m
	}

	full := &plan.CallProc{
		Kind:      plan.ProcPropagate,
		Seeds:     []graph.NodeID{graph.NodeID(n0)},
		SeedVals:  []float64{10.0},
		RelTypes:  []string{"R"},
		Direction: chickpeas.Outgoing,
		MaxDepth:  5,
		ValueProp: "w",
		MinValue:  math.Inf(-1),
	}
	m := byNode(callPropagateRows(full, g))
	// The seed carries its own value at depth 1; each claimed node takes the
	// claiming rel's ValueProp; the isolated node is never reached.
	if len(m) != 3 {
		t.Fatalf("reached %d nodes, want 3 (isolated node excluded)", len(m))
	}
	if r := m[graph.NodeID(n0)]; r.Value != 10 || r.Depth != 1 {
		t.Fatalf("seed = %+v, want value 10 depth 1", r)
	}
	if r := m[graph.NodeID(n1)]; r.Value != 5 || r.Depth != 2 {
		t.Fatalf("n1 = %+v, want value 5 depth 2", r)
	}
	if r := m[graph.NodeID(n2)]; r.Value != 7 || r.Depth != 3 {
		t.Fatalf("n2 = %+v, want value 7 depth 3", r)
	}

	// MaxDepth 1 keeps seeds only (they sit at depth 1 and expand only while
	// depth < MaxDepth).
	seedsOnly := *full
	seedsOnly.MaxDepth = 1
	if m := byNode(callPropagateRows(&seedsOnly, g)); len(m) != 1 || m[graph.NodeID(n0)].Value != 10 {
		t.Fatalf("MaxDepth 1 reached %d nodes, want the seed only", len(m))
	}
}
