package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// capabilityGraph builds two disjoint R edges (0->1, 2->3) so R is
// functional outgoing (each source has at most one) but not a collapsible
// chain.
func capabilityGraph(t *testing.T) *graph.SnapshotGraph {
	t.Helper()
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
	n3, _ := b.AddNode("N")
	_, err := b.AddRel(n0, n1, "R")
	must(err)
	_, err = b.AddRel(n2, n3, "R")
	must(err)
	return graph.New(b.Finalize("cap"))
}

// TestChainFuncFor covers the per-op functionality memo: an outgoing single
// type resolves through FunctionalVia and caches, a Both direction
// short-circuits to false without querying, and repeat calls are memoized.
func TestChainFuncFor(t *testing.T) {
	ctx := &eval.Ctx{G: capabilityGraph(t)}
	var s genScratch

	out := &plan.BindOp{Types: []string{"R"}, Dir: chickpeas.Outgoing}
	if !s.chainFuncFor(ctx, out) {
		t.Fatal("R is functional outgoing (each source has at most one)")
	}
	if v, ok := s.chainFunc[out]; !ok || !v {
		t.Fatal("outgoing result must be memoized as true")
	}
	if !s.chainFuncFor(ctx, out) {
		t.Fatal("memoized repeat must still be true")
	}

	// A Both direction is never functional (short-circuits, no query).
	both := &plan.BindOp{Types: []string{"R"}, Dir: chickpeas.Both}
	if s.chainFuncFor(ctx, both) {
		t.Fatal("Both direction must not be functional")
	}
	if v, ok := s.chainFunc[both]; !ok || v {
		t.Fatal("Both result must be memoized as false")
	}
}

// TestChainRootsFor covers the per-op chain-collapse memo: a non-collapsible
// walk resolves to (nil, false) and is cached (the map entry exists so the
// second call skips the resolve).
func TestChainRootsFor(t *testing.T) {
	ctx := &eval.Ctx{G: capabilityGraph(t)}
	var s genScratch

	op := &plan.BindOp{Types: []string{"R"}, Dir: chickpeas.Outgoing, Labels: []string{"N"}}
	roots, ok := s.chainRootsFor(ctx, op)
	if ok || roots != nil {
		t.Fatalf("non-collapsible walk = %v,%v, want nil,false", roots, ok)
	}
	if _, seen := s.chainRoots[op]; !seen {
		t.Fatal("the resolve must be memoized even when it returns nil")
	}
	if roots2, ok2 := s.chainRootsFor(ctx, op); ok2 || roots2 != nil {
		t.Fatalf("memoized repeat = %v,%v, want nil,false", roots2, ok2)
	}
}

// TestBaseScanKind covers the ScanExistsSeed degradation source: a label
// scan when a label exists, else every node.
func TestBaseScanKind(t *testing.T) {
	if got := baseScanKind(&plan.ScanSource{Label: "N"}); got != plan.ScanLabel {
		t.Fatalf("labeled = %v, want ScanLabel", got)
	}
	if got := baseScanKind(&plan.ScanSource{Label: ""}); got != plan.ScanAll {
		t.Fatalf("unlabeled = %v, want ScanAll", got)
	}
}

// TestRelSlotOf covers the relationship-slot accessor: a single-hop expand
// reports its rel slot, while every other op kind (including a var-expand,
// whose per-trail rel list is not a single bound slot) reports NoSlot.
func TestRelSlotOf(t *testing.T) {
	if got := relSlotOf(&plan.BindOp{Kind: plan.OpExpand, RelSlot: 4}); got != 4 {
		t.Fatalf("relSlotOf(expand) = %d, want 4", got)
	}
	for _, k := range []plan.OpKind{plan.OpScan, plan.OpVarExpand} {
		if got := relSlotOf(&plan.BindOp{Kind: k, RelSlot: 4}); got != plan.NoSlot {
			t.Fatalf("relSlotOf(kind %v) = %d, want NoSlot", k, got)
		}
	}
}
