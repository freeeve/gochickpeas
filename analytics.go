// Whole-graph analytics: PageRank, weakly-connected components, community
// detection by label propagation (CDLP), the local clustering coefficient
// (LCC), and single-source shortest paths (SSSP) -- the LDBC Graphalytics
// kernel set. Outputs are node-indexed slices over the CSR id space (index
// == node id; sparse-id safe). directed selects the forward direction:
// true follows Outgoing (a directed graph), false Both (an undirected one
// whose rels are stored once).

package chickpeas

import (
	"math"
	"sort"

	"github.com/freeeve/gochickpeas/parallel"
)

func fwd(directed bool) Direction {
	if directed {
		return Outgoing
	}
	return Both
}

func ind(directed bool) Direction {
	if directed {
		return Incoming
	}
	return Both
}

// SSSP is single-source shortest paths over forward rels with additive
// weights from the weightKey rel property ("" = unit weights); unreachable
// nodes get +Inf. Unit weights make every distance the hop count, so that
// case runs the BFS -- a unit-weight Dijkstra pays a heap push and pop
// plus map probes per relationship to rediscover the ordering a BFS queue
// gives for free (the same dispatch the COST-constant path search takes).
// A weight key that resolves no column also means unit weights.
func (g *Snapshot) SSSP(source NodeID, directed bool, weightKey string) []float64 {
	out := make([]float64, g.CSRIDSpace())
	var weight WeightFn
	if weightKey != "" {
		if col, ok := g.RelCol(weightKey); ok {
			f64s := col.F64()
			weight = func(_ NodeID, rel RelRef) float64 {
				if w, ok := f64s.Get(rel.Pos); ok {
					return w
				}
				return 1
			}
		}
	}
	if weight == nil {
		dists := g.BFSDistances(source, fwd(directed), MatchAll(), -1)
		for v := range out {
			if d, ok := dists[NodeID(v)]; ok {
				out[v] = float64(d)
			} else {
				out[v] = math.Inf(1)
			}
		}
		return out
	}
	sp := g.Dijkstra(source, fwd(directed), MatchAll(), weight)
	for v := range out {
		if d, ok := sp.Distance(NodeID(v)); ok {
			out[v] = d
		} else {
			out[v] = math.Inf(1)
		}
	}
	return out
}

// WCC labels each node with the smallest node id in its weakly-connected
// component, flooding undirected (Both) rels in ascending id sweep order.
func (g *Snapshot) WCC() []uint32 {
	return g.wccFlood(MatchAll(), Both)
}

// WCCVia is connected components over only the m-matched rels, flooding in
// dir (pass Both for the usual weakly-connected sense over a
// stored-directed rel set, e.g. a reply forest). Nodes with no matching
// rel are their own singleton component.
func (g *Snapshot) WCCVia(m RelMatch, dir Direction) []uint32 {
	return g.wccFlood(m, dir)
}

func (g *Snapshot) wccFlood(m RelMatch, dir Direction) []uint32 {
	n := uint32(g.CSRIDSpace())
	const unvisited = ^uint32(0)
	comp := make([]uint32, n)
	for i := range comp {
		comp[i] = unvisited
	}
	var stack []uint32
	for s := uint32(0); s < n; s++ {
		if comp[s] != unvisited {
			continue
		}
		comp[s] = s
		stack = append(stack[:0], s)
		for len(stack) > 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for u := range g.NeighborsMatch(v, dir, m) {
				if comp[u] == unvisited {
					comp[u] = s
					stack = append(stack, u)
				}
			}
		}
	}
	return comp
}

// PageRank runs iterations of synchronous pull updates with the given
// damping: PR0(v) = 1/|V|, then PRi(v) = (1-d)/|V| + d*(sum over in-
// neighbors of PRi-1(u)/outdeg(u) + the uniformly redistributed rank of
// sinks).
func (g *Snapshot) PageRank(directed bool, damping float64, iterations int) []float64 {
	n := int(g.CSRIDSpace())
	if n == 0 {
		return nil
	}
	nf := float64(n)
	out, in := fwd(directed), ind(directed)
	all := MatchAll()
	outdeg := make([]uint32, n)
	parallel.For(n, func(lo, hi int) {
		for v := lo; v < hi; v++ {
			d := uint32(0)
			for range g.NeighborsMatch(NodeID(v), out, all) {
				d++
			}
			outdeg[v] = d
		}
	})
	pr := make([]float64, n)
	next := make([]float64, n)
	for i := range pr {
		pr[i] = 1 / nf
	}
	for range iterations {
		dangling := 0.0
		for v := range n {
			if outdeg[v] == 0 {
				dangling += pr[v]
			}
		}
		base := (1-damping)/nf + damping*dangling/nf
		parallel.For(n, func(lo, hi int) {
			for v := lo; v < hi; v++ {
				pull := 0.0
				for u := range g.NeighborsMatch(NodeID(v), in, all) {
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

// CDLP is community detection by synchronous label propagation: L0(v) = v,
// then each node adopts the most frequent label among its neighbors
// (in+out tallied separately for directed graphs, so a mutual rel counts
// twice), smallest label breaking ties; a node with no neighbors keeps its
// label.
func (g *Snapshot) CDLP(directed bool, iterations int) []uint32 {
	init := make([]uint32, g.CSRIDSpace())
	for i := range init {
		init[i] = uint32(i)
	}
	return g.CDLPSeeded(directed, iterations, init)
}

// CDLPSeeded is CDLP with explicit initial labels (init[node] = L0(node));
// seed with original vertex ids to match a vertex-id-keyed reference.
// Missing slots (a short init) default to the node's own id.
func (g *Snapshot) CDLPSeeded(directed bool, iterations int, init []uint32) []uint32 {
	n := int(g.CSRIDSpace())
	cur := make([]uint32, n)
	for v := range n {
		if v < len(init) {
			cur[v] = init[v]
		} else {
			cur[v] = uint32(v)
		}
	}
	next := make([]uint32, n)
	all := MatchAll()
	for range iterations {
		parallel.For(n, func(lo, hi int) {
			// One label-tally scratch per chunk, reused across its nodes.
			var buf []uint32
			for v := lo; v < hi; v++ {
				buf = buf[:0]
				if directed {
					for u := range g.NeighborsMatch(NodeID(v), Outgoing, all) {
						buf = append(buf, cur[u])
					}
					for u := range g.NeighborsMatch(NodeID(v), Incoming, all) {
						buf = append(buf, cur[u])
					}
				} else {
					for u := range g.NeighborsMatch(NodeID(v), Both, all) {
						buf = append(buf, cur[u])
					}
				}
				next[v] = mostFrequentLabel(buf, cur[v])
			}
		})
		cur, next = next, cur
	}
	return cur
}

// mostFrequentLabel is the most frequent value of buf (smallest on a tie,
// via sort + run scan), or fallback when buf is empty.
func mostFrequentLabel(buf []uint32, fallback uint32) uint32 {
	if len(buf) == 0 {
		return fallback
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
	best, bestCount := fallback, 0
	for i := 0; i < len(buf); {
		j := i + 1
		for j < len(buf) && buf[j] == buf[i] {
			j++
		}
		if j-i > bestCount {
			bestCount = j - i
			best = buf[i]
		}
		i = j
	}
	return best
}

// LCC is the local clustering coefficient: for each node v with undirected
// neighbor set N(v) (each neighbor once, self excluded), 0 if |N(v)| <= 1,
// else the number of forward rels between members of N(v) over
// |N(v)|*(|N(v)|-1).
func (g *Snapshot) LCC(directed bool) []float64 {
	n := int(g.CSRIDSpace())
	out := fwd(directed)
	all := MatchAll()

	// Sorted forward adjacency, built once, so the triangle count probes
	// the smaller side of each intersection.
	off := make([]uint32, n+1)
	for v := range n {
		d := uint32(0)
		for range g.NeighborsMatch(NodeID(v), out, all) {
			d++
		}
		off[v+1] = off[v] + d
	}
	adj := make([]uint32, off[n])
	for v := range n {
		p := off[v]
		for w := range g.NeighborsMatch(NodeID(v), out, all) {
			adj[p] = w
			p++
		}
		s := adj[off[v]:p]
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}

	result := make([]float64, n)
	parallel.For(n, func(lo, hi int) {
		mark := make([]uint64, (n+63)/64)
		var nbrs []uint32
		for v := lo; v < hi; v++ {
			nbrs = nbrs[:0]
			for u := range g.NeighborsMatch(NodeID(v), Both, all) {
				marked := mark[u>>6]>>(u&63)&1 != 0
				if u != NodeID(v) && !marked {
					mark[u>>6] |= 1 << (u & 63)
					nbrs = append(nbrs, u)
				}
			}
			k := len(nbrs)
			if k >= 2 {
				needSort := false
				for _, u := range nbrs {
					if int(off[u+1]-off[u]) > k {
						needSort = true
						break
					}
				}
				if needSort {
					sort.Slice(nbrs, func(i, j int) bool { return nbrs[i] < nbrs[j] })
				}
				rels := uint64(0)
				for _, u := range nbrs {
					uo := adj[off[u]:off[u+1]]
					if len(uo) <= k {
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
							found, pos := gallop(uo, cursor, w)
							cursor = pos
							if found {
								rels++
							}
						}
					}
				}
				result[v] = float64(rels) / (float64(k) * float64(k-1))
			}
			for _, u := range nbrs {
				mark[u>>6] &^= 1 << (u & 63)
			}
		}
	})
	return result
}

// gallop is exponential search for target in uo[from:]: whether present
// and the index where it is/would be (always >= from); the cursor only
// advances.
func gallop(uo []uint32, from int, target uint32) (bool, int) {
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
	i := sort.Search(hi-lo, func(i int) bool { return uo[lo+i] >= target })
	if lo+i < hi && uo[lo+i] == target {
		return true, lo + i
	}
	return false, lo + i
}
