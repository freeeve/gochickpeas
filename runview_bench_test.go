// Benchmark for the below-floor run-view lookup: hub nodes (mixed degree
// past the run-scan gate) traversed through a sparse type, so every probe
// resolves a run in the payload-proportional view. Public-API only, so it
// A/Bs unchanged across the lookup's implementations.
package chickpeas_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func BenchmarkRunViewLookup(b *testing.B) {
	const (
		n    = 1 << 20 // id space large enough that SPARSE sits far below the typed floor
		hubs = 10_000
	)
	bl := chickpeas.NewBuilder(n, hubs*73)
	for i := 0; i < n; i++ {
		if _, err := bl.AddNodeWithID(uint32(i), "N"); err != nil {
			b.Fatal(err)
		}
	}
	// Each hub: 70 BULK rels push its mixed degree past the run-scan gate,
	// 3 SPARSE rels give it a short run in the sparse view (30k SPARSE rels
	// total, well under idspace/4 -- the below-floor route).
	seed := uint64(0xC0FFEE)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for h := 0; h < hubs; h++ {
		u := chickpeas.NodeID(h * (n / hubs))
		for range 70 {
			if _, err := bl.AddRel(u, chickpeas.NodeID(next()%n), "BULK"); err != nil {
				b.Fatal(err)
			}
		}
		for range 3 {
			if _, err := bl.AddRel(u, chickpeas.NodeID(next()%n), "SPARSE"); err != nil {
				b.Fatal(err)
			}
		}
	}
	g := bl.Finalize()
	m := g.Match("SPARSE")
	var dst []chickpeas.NodeID
	// Warm the lazy view outside the timed loop.
	dst = g.AppendNeighborsEach(dst[:0], 0, chickpeas.Outgoing, m)
	b.ResetTimer()
	for b.Loop() {
		total := 0
		for h := 0; h < hubs; h++ {
			u := chickpeas.NodeID(h * (n / hubs))
			dst = g.AppendNeighborsEach(dst[:0], u, chickpeas.Outgoing, m)
			total += len(dst)
		}
		if total != 3*hubs {
			b.Fatalf("total = %d, want %d", total, 3*hubs)
		}
	}
}
