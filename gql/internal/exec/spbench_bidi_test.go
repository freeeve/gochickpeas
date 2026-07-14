// Benchmark for the single-pair shortest-path search in the regime the
// bidirectional walk targets: one endpoint adjacent to a hub, the other on
// a sparse tail -- cost is edges touched, so expanding the smaller
// frontier must never expand the hub's ply. Untracked-compatible: public
// scratch and search signatures only.
package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// hubTailGraph: source 0 -> hub 1 -> {2..fanout+1}, and a 10-deep chain
// hanging off node 2 whose tail is the target.
func hubTailGraph(b *testing.B, fanout int) (*eval.Ctx, graph.NodeID) {
	b.Helper()
	const tail = 10
	n := fanout + tail + 4
	bl := chickpeas.NewBuilder(n, fanout+tail+4)
	for range n {
		if _, err := bl.AddNode("N"); err != nil {
			b.Fatal(err)
		}
	}
	must := func(err error) {
		if err != nil {
			b.Fatal(err)
		}
	}
	_, err := bl.AddRel(0, 1, "R")
	must(err)
	for i := 2; i < fanout+2; i++ {
		_, err := bl.AddRel(1, chickpeas.NodeID(i), "R")
		must(err)
	}
	cur := chickpeas.NodeID(2)
	for i := 0; i < tail; i++ {
		nxt := chickpeas.NodeID(fanout + 2 + i)
		_, err := bl.AddRel(cur, nxt, "R")
		must(err)
		cur = nxt
	}
	return &eval.Ctx{G: graph.New(bl.Finalize())}, graph.NodeID(cur)
}

func BenchmarkShortestPathHubTail(b *testing.B) {
	ctx, target := hubTailGraph(b, 50_000)
	rm := ctx.G.CompileRelMatcher([]string{"R"})
	sp := &plan.SpStage{Dir: graph.Outgoing, Types: []string{"R"}}
	scr := newSPScratch()
	b.ResetTimer()
	for b.Loop() {
		p, found := shortestPath(ctx, 0, target, sp, rm, nil, scr)
		if !found || len(p.nodes) != 13 {
			b.Fatalf("found=%v len=%d", found, len(p.nodes))
		}
	}
}
