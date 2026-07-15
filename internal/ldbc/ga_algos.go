// The six Graphalytics algorithms (LDBC Graphalytics spec v1.0.x) --
// ports of rustychickpeas-ldbc src/graphalytics/mod.rs. Outputs are
// node-indexed dense arrays; SSSP wraps the core Dijkstra, the rest
// run directly over the snapshot adjacency. The parallel loops write
// disjoint index ranges, so results are deterministic.

package ldbc

import (
	"math"
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/parallel"
)

// gaFwd is the forward rel direction: outgoing for a directed graph,
// both for an undirected one (whose rels are stored once).
func gaFwd(directed bool) chickpeas.Direction {
	if directed {
		return chickpeas.Outgoing
	}
	return chickpeas.Both
}

// GABFS is breadth-first depth from source over forward rels;
// unreachable nodes get MaxInt64, per the spec.
func GABFS(g *chickpeas.Snapshot, source uint32, directed bool) []int64 {
	n := g.NodeCount()
	dir := gaFwd(directed)
	dist := make([]int64, n)
	for i := range dist {
		dist[i] = math.MaxInt64
	}
	if source >= n {
		return dist
	}
	dist[source] = 0
	queue := make([]uint32, 0, n)
	queue = append(queue, source)
	for head := 0; head < len(queue); head++ {
		u := queue[head]
		du := dist[u]
		for w := range g.Neighbors(u, dir) {
			if dist[w] == math.MaxInt64 {
				dist[w] = du + 1
				queue = append(queue, w)
			}
		}
	}
	return dist
}

// GASSSP is single-source shortest paths over forward rels (weight rel
// property, absent = 1.0); unreachable nodes get +Inf.
//
// With NO weight column every edge is unit weight, so the shortest paths are
// exactly BFS hop counts -- the unit-weight-Dijkstra-is-a-BFS principle
// (tasks 126/128/140). Dispatch to the allocation-lean BFS and lift to float
// instead of running the full weighted Dijkstra, whose per-node settle/heap
// state allocated ~3x per node on a large graph. A present weight column (even
// with some missing values, which fall back to 1.0) stays weighted Dijkstra.
func GASSSP(g *chickpeas.Snapshot, source uint32, directed bool) []float64 {
	c, ok := g.RelColIndexed("weight")
	if !ok {
		hops := GABFS(g, source, directed)
		out := make([]float64, len(hops))
		for i, h := range hops {
			if h == math.MaxInt64 {
				out[i] = math.Inf(1)
			} else {
				out[i] = float64(h)
			}
		}
		return out
	}
	wcol := c.F64()
	weight := func(_ chickpeas.NodeID, rel chickpeas.RelRef) float64 {
		if w, ok := wcol.Get(rel.Pos); ok {
			return w
		}
		return 1.0
	}
	sp := g.Dijkstra(source, gaFwd(directed), chickpeas.MatchAll(), weight)
	n := g.NodeCount()
	out := make([]float64, n)
	for v := range n {
		if d, ok := sp.Distance(v); ok {
			out[v] = d
		} else {
			out[v] = math.Inf(1)
		}
	}
	return out
}

// GAWCC labels each node with the smallest node id in its weakly
// connected component (undirected flood in ascending sweep order).
func GAWCC(g *chickpeas.Snapshot) []uint32 {
	n := g.NodeCount()
	comp := make([]uint32, n)
	for i := range comp {
		comp[i] = math.MaxUint32
	}
	var stack []uint32
	for s := range n {
		if comp[s] != math.MaxUint32 {
			continue
		}
		comp[s] = s
		stack = append(stack[:0], s)
		for len(stack) > 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for u := range g.Neighbors(v, chickpeas.Both) {
				if comp[u] == math.MaxUint32 {
					comp[u] = s
					stack = append(stack, u)
				}
			}
		}
	}
	return comp
}

// GAPageRank runs `iterations` synchronous updates with the given
// damping; sinks redistribute their rank uniformly. Pull formulation:
// each node sums its in-neighbours' shares into a disjoint slot.
func GAPageRank(g *chickpeas.Snapshot, directed bool, damping float64, iterations int) []float64 {
	n := int(g.NodeCount())
	if n == 0 {
		return nil
	}
	nf := float64(n)
	out := gaFwd(directed)
	inDir := chickpeas.Incoming
	if !directed {
		inDir = chickpeas.Both
	}
	outdeg := make([]uint32, n)
	parallel.For(n, func(lo, hi int) {
		for v := lo; v < hi; v++ {
			var d uint32
			for range g.Neighbors(uint32(v), out) {
				d++
			}
			outdeg[v] = d
		}
	})
	pr := make([]float64, n)
	for i := range pr {
		pr[i] = 1.0 / nf
	}
	next := make([]float64, n)
	for range iterations {
		var dangling float64
		for v := range n {
			if outdeg[v] == 0 {
				dangling += pr[v]
			}
		}
		base := (1.0-damping)/nf + damping*dangling/nf
		parallel.For(n, func(lo, hi int) {
			for v := lo; v < hi; v++ {
				var pull float64
				for u := range g.Neighbors(uint32(v), inDir) {
					if d := outdeg[u]; d > 0 {
						pull += pr[u] / float64(d)
					}
				}
				next[v] = base + damping*pull
			}
		})
		pr, next = next, pr
	}
	return pr
}

// GACDLPSeeded is synchronous label propagation from explicit initial
// labels (seed with original vertex ids so the smallest-label tie-break
// operates in vertex-id space, matching the references). Directed
// graphs tally incoming and outgoing separately, so a mutual rel counts
// twice.
func GACDLPSeeded(g *chickpeas.Snapshot, directed bool, iterations int, init []uint32) []uint32 {
	n := int(g.NodeCount())
	cur := slices.Clone(init)
	next := make([]uint32, n)
	// Per-worker neighbour-label scratch, persistent across every pass:
	// worker w owns scratch[w], reused pass to pass so it grows once (to its
	// chunk's peak degree) instead of re-allocating from nil on every
	// iteration -- that per-pass regrowth dominated CDLP's allocations.
	// ForWorker's stable index keys the scratch, and a given index is used at
	// most once per pass (passes run sequentially), so the reuse is race-free.
	scratch := make([][]chickpeas.NodeID, parallel.Chunks(n))
	all := chickpeas.MatchAll()
	for range iterations {
		parallel.ForWorker(n, func(worker, lo, hi int) {
			// Neighbors batch-append through the CSR seam (no per-element
			// yield), then rewrite to labels in place -- NodeID and the label
			// are both uint32, so one buffer serves both stages.
			buf := scratch[worker]
			for v := lo; v < hi; v++ {
				buf = buf[:0]
				if directed {
					buf = g.AppendNeighborsEach(buf, chickpeas.NodeID(v), chickpeas.Outgoing, all)
					buf = g.AppendNeighborsEach(buf, chickpeas.NodeID(v), chickpeas.Incoming, all)
				} else {
					buf = g.AppendNeighborsEach(buf, chickpeas.NodeID(v), chickpeas.Both, all)
				}
				for i, u := range buf {
					buf[i] = chickpeas.NodeID(cur[u])
				}
				slices.Sort(buf)
				best, bestCount := cur[v], 0
				for i := 0; i < len(buf); {
					lab := buf[i]
					j := i + 1
					for j < len(buf) && buf[j] == lab {
						j++
					}
					if j-i > bestCount {
						bestCount = j - i
						best = lab
					}
					i = j
				}
				next[v] = best
			}
			scratch[worker] = buf // persist grown capacity for the next pass
		})
		cur, next = next, cur
	}
	return cur
}

// GALCC is the local clustering coefficient: rels among each node's
// undirected neighbour set over k*(k-1), forward rels counting.
func GALCC(g *chickpeas.Snapshot, directed bool) []float64 {
	n := int(g.NodeCount())
	out := gaFwd(directed)

	// Sorted forward-adjacency (CSR), built once; the triangle count
	// probes the smaller side of each intersection.
	off := make([]uint32, n+1)
	for v := range n {
		var d uint32
		for range g.Neighbors(uint32(v), out) {
			d++
		}
		off[v+1] = off[v] + d
	}
	adj := make([]uint32, off[n])
	for v := range n {
		s := off[v]
		p := s
		for w := range g.Neighbors(uint32(v), out) {
			adj[p] = w
			p++
		}
		slices.Sort(adj[s:p])
	}

	result := make([]float64, n)
	nWords := (n + 63) / 64
	parallel.For(n, func(lo, hi int) {
		// Per-chunk scratch: the N(v) membership bitset and neighbour
		// buffer, reused across the chunk's nodes.
		mark := make([]uint64, nWords)
		var nbrs, raw []uint32
		all := chickpeas.MatchAll()
		// A membership probe against mark is one load and mask on a
		// sequential scan; each binary-search level in the gallop pays a
		// random cache miss. Scanning u's adjacency therefore wins until
		// it is far larger than the probe set -- gallop only past this
		// ratio.
		const gallopRatio = 32
		for v := lo; v < hi; v++ {
			raw = g.AppendNeighborsEach(raw[:0], uint32(v), chickpeas.Both, all)
			nbrs = nbrs[:0]
			for _, u := range raw {
				marked := mark[u>>6]>>(u&63)&1 != 0
				if u != uint32(v) && !marked {
					mark[u>>6] |= 1 << (u & 63)
					nbrs = append(nbrs, u)
				}
			}
			k := len(nbrs)
			if k >= 2 {
				needSort := false
				for _, u := range nbrs {
					if int(off[u+1]-off[u]) > gallopRatio*k {
						needSort = true
						break
					}
				}
				if needSort {
					slices.Sort(nbrs)
				}
				var rels uint64
				for _, u := range nbrs {
					uo := adj[off[u]:off[u+1]]
					if len(uo) <= gallopRatio*k {
						for _, w := range uo {
							if w != u && mark[w>>6]>>(w&63)&1 != 0 {
								rels++
							}
						}
					} else {
						cursor := 0
						for _, w := range nbrs {
							if w == u {
								continue
							}
							found, pos := gaGallop(uo, cursor, w)
							cursor = pos
							if found {
								rels++
							}
						}
					}
				}
				result[v] = float64(rels) / (float64(k) * (float64(k) - 1.0))
			}
			for _, u := range nbrs {
				mark[u>>6] &^= 1 << (u & 63)
			}
		}
	})
	return result
}

// gaGallop is exponential search for target in uo[from:]: whether it is
// present and the index where it is or would be inserted (>= from).
// Successive calls walk uo monotonically.
func gaGallop(uo []uint32, from int, target uint32) (bool, int) {
	n := len(uo)
	if from >= n {
		return false, n
	}
	bound := 1
	for from+bound < n && uo[from+bound] < target {
		bound *= 2
	}
	lo := from + bound/2
	hi := min(from+bound+1, n)
	i, found := slices.BinarySearch(uo[lo:hi], target)
	return found, lo + i
}
