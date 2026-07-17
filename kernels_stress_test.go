package chickpeas_test

// CommonNeighborCounts chunk-slab stress (task 203): the pair primitive
// accumulates per-worker fixed-size slabs sealed and concatenated in
// worker order. The hazards: dropped open slabs at merge, order breaks
// at worker/chunk boundaries, and count drift under duplicate-heavy
// fan-in. Verified against a naive single-threaded reference across
// source counts straddling the worker count and pair volumes straddling
// the 4096 slab size.

import (
	"fmt"
	"math/rand"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

func TestCommonNeighborCountsSlabBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	// Random bipartite-ish multigraph: sources fan to mids, mids fan to
	// targets, with parallel edges and self-ish structure for dedup
	// pressure.
	const nNodes = 800
	b := chickpeas.NewBuilder(nNodes, nNodes*40)
	ids := make([]chickpeas.NodeID, nNodes)
	for i := range ids {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = nd
	}
	for e := 0; e < nNodes*30; e++ {
		x := ids[rng.Intn(nNodes)]
		y := ids[rng.Intn(nNodes)]
		if _, err := b.AddRel(x, y, "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("cnc-stress")
	m := g.Match("R")

	targets := nodeset.New()
	for i := 0; i < nNodes; i += 2 {
		targets.Insert(uint32(ids[i]))
	}

	// Naive reference: per source, distinct mids; per (source, target),
	// count of distinct mids adjacent to both.
	reference := func(sources []chickpeas.NodeID) map[[2]uint32]uint64 {
		out := map[[2]uint32]uint64{}
		for _, s := range sources {
			mids := map[chickpeas.NodeID]bool{}
			for mid := range g.NeighborsMatch(s, chickpeas.Both, m) {
				mids[mid] = true
			}
			for mid := range mids {
				ts := map[chickpeas.NodeID]bool{}
				for tt := range g.NeighborsMatch(mid, chickpeas.Both, m) {
					if targets.Contains(uint32(tt)) {
						ts[tt] = true
					}
				}
				for tt := range ts {
					out[[2]uint32{uint32(s), uint32(tt)}]++
				}
			}
		}
		return out
	}

	// Source counts straddling worker boundaries and slab volumes: 0, 1,
	// then bands around typical worker counts and a full sweep whose pair
	// volume crosses several 4096 slabs.
	for _, nSrc := range []int{0, 1, 2, 15, 16, 17, 63, 64, 65, 400, 800} {
		sources := make([]chickpeas.NodeID, 0, nSrc)
		for i := 0; i < nSrc; i++ {
			sources = append(sources, ids[i%nNodes])
		}
		got := g.CommonNeighborCounts(sources, chickpeas.Both, m, targets)
		want := reference(sources)
		// Duplicate sources emit duplicate groups: fold got by summing
		// per occurrence count, comparing multiset semantics per source
		// OCCURRENCE. Reference counts per occurrence too (loop above
		// iterates the same duplicated slice), so totals must agree.
		gotTotal := map[[2]uint32]uint64{}
		for _, p := range got {
			gotTotal[[2]uint32{uint32(p.Source), uint32(p.Target)}] += p.Count
		}
		if len(gotTotal) != len(want) {
			t.Fatalf("nSrc=%d: %d distinct pairs, want %d", nSrc, len(gotTotal), len(want))
		}
		for k, w := range want {
			if gotTotal[k] != w {
				t.Fatalf("nSrc=%d: pair %v count %d, want %d", nSrc, k, gotTotal[k], w)
			}
		}
		// Source-contiguity contract: each source's pairs appear as one
		// contiguous run (callers rebuild per-source filters on change).
		seen := map[chickpeas.NodeID]bool{}
		var last chickpeas.NodeID
		have := false
		for _, p := range got {
			if !have || p.Source != last {
				if seen[p.Source] && countOccurrences(sources, p.Source) == 1 {
					t.Fatalf("nSrc=%d: source %d pairs not contiguous", nSrc, p.Source)
				}
				seen[p.Source] = true
				last, have = p.Source, true
			}
		}
		_ = fmt.Sprintf
	}
}

func countOccurrences(sources []chickpeas.NodeID, s chickpeas.NodeID) int {
	n := 0
	for _, x := range sources {
		if x == s {
			n++
		}
	}
	return n
}
