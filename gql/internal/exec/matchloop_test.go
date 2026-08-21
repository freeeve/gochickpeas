package exec

import (
	"runtime"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestGenMatchesEntryScratchDoesNotAllocPerRow pins the
// once-per-call-is-per-row trap: a chained MATCH stage calls genMatches
// once per pushed row, so its entry-time level buffers (pos, uniqPushed,
// swept) must reset by reuse, not by make -- the pre-fix form cost three
// allocations per incoming row. 2,000 rows through a second stage must
// stay far under one alloc per row for the whole execution.
func TestGenMatchesEntryScratchDoesNotAllocPerRow(t *testing.T) {
	const nRows = 2000
	b := chickpeas.NewBuilder(nRows+8, nRows+8)
	for i := 0; i < nRows; i++ {
		a, err := b.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.AddNode("C")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddRel(a, c, "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(b.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:A) MATCH (a)-[:R]->(c:C) RETURN count(*) AS n")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		if _, err := Execute(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	run() // warm: lazy caches, scratch growth
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	if allocs := after.Mallocs - before.Mallocs; allocs > nRows/2 {
		t.Fatalf("warm run allocated %d times over %d chained rows -- entry scratch is allocating per row", allocs, nRows)
	}
}

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
	// Both expand kinds report their relationship slot -- the var-length
	// LIST slot must be tracked for pushdown placement, or a conjunct
	// reading it lands where the slot is still null (task 298).
	for _, k := range []plan.OpKind{plan.OpExpand, plan.OpVarExpand} {
		if got := relSlotOf(&plan.BindOp{Kind: k, RelSlot: 4}); got != 4 {
			t.Fatalf("relSlotOf(kind %v) = %d, want 4", k, got)
		}
	}
	if got := relSlotOf(&plan.BindOp{Kind: plan.OpScan, RelSlot: 4}); got != plan.NoSlot {
		t.Fatalf("relSlotOf(scan) = %d, want NoSlot", got)
	}
}

// TestScanMemoReusesRowIndependentScans locks the loop-invariant scan
// memo (task 315, after rustychickpeas's correction to the twin-probe
// cost claim): a row-independent scan nested below the driving level
// used to re-run for every outer row -- with the numeric-twin probes
// that is two index probes per row, and for an unselective anchor the
// recomputation dwarfs the probes. The memo fills once per stage
// execution and later rows copy it. Engagement is asserted through the
// hit counter, identity through the disable switch.
func TestScanMemoReusesRowIndependentScans(t *testing.T) {
	b := chickpeas.NewBuilder(16, 0)
	for range 3 {
		if _, err := b.AddNode("A"); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		id, err := b.AddNode("B")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(id, "k", int64(1)); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(b.Finalize("k"))
	q := "MATCH (a:A) MATCH (b:B {k: 1}) RETURN a, b"

	run := func() [][]value.Value {
		t.Helper()
		qq, err := parser.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		pl, err := plan.Build(qq, g)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := Execute(&eval.Ctx{G: g}, pl)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	scanMemoHits = 0
	memo := run()
	if len(memo) != 12 {
		t.Fatalf("rows = %d, want 12 (3 a x 4 b)", len(memo))
	}
	if scanMemoHits == 0 {
		t.Fatal("scan memo never engaged -- the nested scan shape did not build, the test is vacuous")
	}

	disableScanMemo = true
	plain := run()
	disableScanMemo = false
	if len(plain) != len(memo) {
		t.Fatalf("disabled rows = %d, memo rows = %d", len(plain), len(memo))
	}
	for i := range memo {
		for j := range memo[i] {
			am, _ := memo[i][j].AsNode()
			ap, _ := plain[i][j].AsNode()
			if am != ap {
				t.Fatalf("row %d col %d: memo node %d, plain node %d -- order must be identical", i, j, am, ap)
			}
		}
	}
}
