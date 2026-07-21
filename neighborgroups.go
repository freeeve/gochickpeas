// NeighborGroups: a lazy query over each source node's typed neighbors,
// grouped by a projected attribute and reduced per source. Nothing runs
// until a terminal (Sizes / TopBySize) is called; reductions run in
// parallel over the sources with projection and counting kept native.

package chickpeas

import (
	"cmp"
	"slices"
	"sync"

	"github.com/freeeve/gochickpeas/parallel"
)

// NeighborGroups is built by Snapshot.NeighborGroups; chain Project, then
// call a terminal.
type NeighborGroups struct {
	g       *Snapshot
	sources []NodeID
	m       RelMatch
	dir     Direction
	project []Step
}

// SourceSize pairs a source node with its largest-cohort size.
type SourceSize struct {
	Source NodeID
	Size   uint32
}

// NeighborGroups starts a grouped-neighbor reduction over each source's
// m-matched neighbors in dir.
func (g *Snapshot) NeighborGroups(sources []NodeID, m RelMatch, dir Direction) *NeighborGroups {
	return &NeighborGroups{g: g, sources: sources, m: m, dir: dir}
}

// Project maps each neighbor to its group node via a chain of
// first-neighbor steps (like Follow). Without a projection, neighbors
// group by their own id (every cohort is 1).
func (n *NeighborGroups) Project(steps ...Step) *NeighborGroups {
	n.project = steps
	return n
}

// Sizes is the raw reduction: per source, the size of its largest cohort.
// Sources with no projectable neighbors yield 0; an unknown rel type or
// projection type yields all-zero sizes. Runs in parallel over the
// sources; output order follows the input sources.
func (n *NeighborGroups) Sizes() []SourceSize {
	out := make([]SourceSize, len(n.sources))
	// Resolve the projection chain once; an unknown type zeroes everything.
	steps := make([]struct {
		dir Direction
		m   RelMatch
	}, len(n.project))
	ok := true
	for i, s := range n.project {
		t, found := n.g.RelType(s.RelType)
		if !found {
			ok = false
			break
		}
		steps[i].dir = s.Dir
		steps[i].m = MatchType(t)
	}
	if !ok {
		for i, src := range n.sources {
			out[i] = SourceSize{Source: src}
		}
		return out
	}
	// One contiguous range per WORKER (not per 4x-oversplit chunk): the
	// per-range scratch is heavy -- a counts map and a neighbors buffer --
	// and both reach a high-water bounded by a single source (counts
	// clears per source, the buffer by max degree), so fewer ranges cost
	// strictly fewer allocations with no growth penalty. Neighbors batch
	// into the reused buffer; the iterator form built a closure per
	// source.
	nsrc := len(n.sources)
	workers := max(min(nsrc, parallel.Workers()), 1)
	size := (nsrc + workers - 1) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		lo, hi := w*size, min((w+1)*size, nsrc)
		go func() {
			defer wg.Done()
			counts := map[NodeID]uint32{}
			var nbrs []NodeID
			for i := lo; i < hi; i++ {
				src := n.sources[i]
				clear(counts)
				nbrs = n.g.AppendNeighborsMatch(nbrs[:0], src, n.dir, n.m)
				for _, nb := range nbrs {
					cur, projected := nb, true
					for _, step := range steps {
						next, found := n.g.FirstNeighborMatch(cur, step.dir, step.m)
						if !found {
							projected = false
							break
						}
						cur = next
					}
					if projected {
						counts[cur]++
					}
				}
				best := uint32(0)
				for _, c := range counts {
					best = max(best, c)
				}
				out[i] = SourceSize{Source: src, Size: best}
			}
		}()
	}
	wg.Wait()
	return out
}

// TopBySize is the n sources with the largest cohorts, size descending.
// Ties break by the tieKey node property (read as i64, ascending) when
// non-empty, else by source id ascending -- always deterministic.
func (n *NeighborGroups) TopBySize(count int, tieKey string) []SourceSize {
	sizes := n.Sizes()
	if tieKey != "" {
		// Resolve each source's tie value once, not inside the comparator.
		ties := make([]int64, len(sizes))
		for i, s := range sizes {
			ties[i] = n.g.Prop(s.Source, tieKey).I64Or(0)
		}
		type keyed struct {
			s   SourceSize
			tie int64
		}
		rows := make([]keyed, len(sizes))
		for i, s := range sizes {
			rows[i] = keyed{s: s, tie: ties[i]}
		}
		// Generic stable sort: sort.SliceStable's reflection-based
		// swapper dominated hot kernels (typedmemmove per swap).
		slices.SortStableFunc(rows, func(a, b keyed) int {
			if a.s.Size != b.s.Size {
				return cmp.Compare(b.s.Size, a.s.Size)
			}
			return cmp.Compare(a.tie, b.tie)
		})
		out := make([]SourceSize, 0, min(count, len(rows)))
		for _, r := range rows[:min(count, len(rows))] {
			out = append(out, r.s)
		}
		return out
	}
	slices.SortStableFunc(sizes, func(a, b SourceSize) int {
		if a.Size != b.Size {
			return cmp.Compare(b.Size, a.Size)
		}
		return cmp.Compare(a.Source, b.Source)
	})
	return sizes[:min(count, len(sizes))]
}
