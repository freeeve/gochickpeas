// NeighborGroups: a lazy query over each source node's typed neighbors,
// grouped by a projected attribute and reduced per source. Nothing runs
// until a terminal (Sizes / TopBySize) is called; reductions run in
// parallel over the sources with projection and counting kept native.

package chickpeas

import (
	"sort"

	"github.com/freeeve/gochickpeas/internal/parallel"
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
	parallel.For(len(n.sources), func(lo, hi int) {
		counts := map[NodeID]uint32{}
		for i := lo; i < hi; i++ {
			src := n.sources[i]
			clear(counts)
			for nb := range n.g.NeighborsMatch(src, n.dir, n.m) {
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
	})
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
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].s.Size != rows[j].s.Size {
				return rows[i].s.Size > rows[j].s.Size
			}
			return rows[i].tie < rows[j].tie
		})
		out := make([]SourceSize, 0, min(count, len(rows)))
		for _, r := range rows[:min(count, len(rows))] {
			out = append(out, r.s)
		}
		return out
	}
	sort.SliceStable(sizes, func(i, j int) bool {
		if sizes[i].Size != sizes[j].Size {
			return sizes[i].Size > sizes[j].Size
		}
		return sizes[i].Source < sizes[j].Source
	})
	return sizes[:min(count, len(sizes))]
}
