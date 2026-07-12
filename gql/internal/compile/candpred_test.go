// Parity tests for the per-candidate predicate specialization: the
// closure must agree with Compiled.Eval's truthiness on every column
// kind x constant kind x operator x operand order, missing properties
// and NaN included.
package compile

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func TestCandidatePredMatchesEval(t *testing.T) {
	b := chickpeas.NewBuilder(16, 4)
	var ids []chickpeas.NodeID
	for i := range 8 {
		n, _ := b.AddNode("N")
		ids = append(ids, n)
		switch i {
		case 0, 1, 2, 3:
			_ = b.SetProp(n, "i", int64(i*10))
			_ = b.SetProp(n, "f", float64(i)*1.5)
			_ = b.SetProp(n, "s", fmt.Sprintf("s%d", i))
		case 4:
			_ = b.SetProp(n, "i", int64(-5))
		case 5:
			_ = b.SetProp(n, "f", 2.5)
			// 6, 7: all props missing
		}
	}
	g := b.Finalize("candpred")
	ctx := &eval.Ctx{G: graph.New(g)}
	slots := map[string]int{"n": 0}
	row := make([]value.Value, 1)

	exprs := []string{
		"n.i = 20", "n.i <> 20", "n.i < 15", "n.i <= 20", "n.i > 10", "n.i >= 30",
		"20 = n.i", "15 > n.i", "10 <= n.i", // reversed operand order
		"n.f < 3.0", "n.f >= 1.5", "3.0 > n.f",
		"n.i < 2.5", "n.f = 2", // mixed int/float pairings
		"n.s = 's1'", "n.s < 's2'", "'s3' >= n.s", // string column
		"n.i = 'x'", "n.f > 'y'", // incomparable kinds
		"n.i IN [10, 30, -5]", "n.s IN ['s0', 's2']", // constant membership
		"n.i IN [10, null]", "n.i IN []", // null element / empty list
	}
	never := func(int) bool { return false }
	for _, src := range exprs {
		// Mirror the level-filter pipeline: hoisting rewrites constant
		// lists to their baked membership form before specialization.
		c := HoistCarriedIn(HoistConstIn(ctx, New(ctx, exprOf(t, src), slots, g), never, nil, slots), never)
		p, ok := CandidatePred(c, 0, slots)
		if !ok {
			t.Fatalf("%q did not specialize (compiled to %T)", src, c.c)
		}
		for _, id := range ids {
			row[0] = value.Node(graph.NodeID(id))
			want := c.Eval(ctx, row, slots).IsTruthy()
			got := p(ctx, row, graph.NodeID(id))
			if got != want {
				t.Fatalf("%q node %d: pred %v, eval %v", src, id, got, want)
			}
		}
	}

	// Carried-list membership: the specialized form must honor the
	// per-epoch rebuild when the carried slot's list changes.
	cslots := map[string]int{"n": 0, "xs": 1}
	crow := make([]value.Value, 2)
	carried := func(s int) bool { return s == 1 }
	cc := HoistCarriedIn(HoistConstIn(ctx, New(ctx, exprOf(t, "n.i IN xs"), cslots, g), never, nil, cslots), carried)
	cp, ok := CandidatePred(cc, 0, cslots)
	if !ok {
		t.Fatalf("carried IN did not specialize (compiled to %T)", cc.c)
	}
	for _, list := range [][]value.Value{
		{value.Int(10), value.Int(-5)},
		{value.Int(30)},
		nil,
	} {
		ctx.MatchEpoch++
		crow[1] = value.List(list)
		for _, id := range ids {
			crow[0] = value.Node(graph.NodeID(id))
			// Pred first: ITS refresh must do the epoch rebuild.
			got := cp(ctx, crow, graph.NodeID(id))
			want := cc.Eval(ctx, crow, cslots).IsTruthy()
			if got != want {
				t.Fatalf("carried IN %v node %d: pred %v, eval %v", list, id, got, want)
			}
		}
	}

	// Bare-variable membership: the element is the candidate itself (the
	// Q4 `topForum2 IN topForums` shape) -- carried node lists, with an
	// epoch change and a non-list epoch.
	np, ok := CandidatePred(HoistCarriedIn(HoistConstIn(ctx, New(ctx, exprOf(t, "n IN xs"), cslots, g), never, nil, cslots), carried), 0, cslots)
	if !ok {
		t.Fatal("bare-variable carried IN did not specialize")
	}
	for _, list := range []value.Value{
		value.List([]value.Value{value.Node(graph.NodeID(ids[1])), value.Node(graph.NodeID(ids[4]))}),
		value.List(nil),
		value.Int(7), // not a list: three-valued null, prunes
	} {
		ctx.MatchEpoch++
		crow[1] = list
		ncc := HoistCarriedIn(HoistConstIn(ctx, New(ctx, exprOf(t, "n IN xs"), cslots, g), never, nil, cslots), carried)
		for _, id := range ids {
			crow[0] = value.Node(graph.NodeID(id))
			got := np(ctx, crow, graph.NodeID(id))
			want := ncc.Eval(ctx, crow, cslots).IsTruthy()
			if got != want {
				t.Fatalf("bare IN %v node %d: pred %v, eval %v", list, id, got, want)
			}
		}
	}

	// Non-specializable shapes must decline: two-slot reads, wrong slot,
	// non-comparison roots.
	for _, src := range []string{"n.i + 1 > 2", "n.i = n.f"} {
		c := New(ctx, exprOf(t, src), slots, g)
		if _, ok := CandidatePred(c, 0, slots); ok {
			t.Fatalf("%q must not specialize", src)
		}
	}
	c := New(ctx, exprOf(t, "n.i = 20"), slots, g)
	if _, ok := CandidatePred(c, 3, slots); ok {
		t.Fatal("wrong slot must not specialize")
	}
}

// TestMembershipDensification pins the adaptive node-membership bitmap:
// past the probe floor the sorted-slice search flips to the dense form,
// and every probe answers identically before, at, and after the flip --
// members, non-members, and ids beyond the bitmap's span.
func TestMembershipDensification(t *testing.T) {
	m := buildMembership([]value.Value{
		value.Node(3), value.Node(64), value.Node(65), value.Node(200),
	})
	if m.kind != memNodes {
		t.Fatalf("kind = %v, want memNodes", m.kind)
	}
	check := func(pass string) {
		t.Helper()
		for _, c := range []struct {
			id  uint32
			hit bool
		}{{3, true}, {64, true}, {65, true}, {200, true},
			{0, false}, {4, false}, {199, false}, {201, false}, {5000, false}} {
			if got := m.hasNode(c.id); got != c.hit {
				t.Fatalf("%s: hasNode(%d) = %v, want %v (dense=%v)", pass, c.id, got, c.hit, m.dense != nil)
			}
		}
	}
	check("sparse")
	for i := uint32(0); i < memDenseProbes+8; i++ {
		m.hasNode(i % 256)
	}
	if m.dense == nil {
		t.Fatal("membership did not densify past the probe floor")
	}
	check("dense")
	// An empty membership never densifies and never hits.
	e := buildMembership([]value.Value{})
	if e.kind == memNodes {
		t.Fatalf("empty list built memNodes")
	}
}
