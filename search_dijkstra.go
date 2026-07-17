// Weighted shortest-path search: single-source Dijkstra with path
// reconstruction, and the bidirectional point-to-point variant that meets
// in the middle.

package chickpeas

import (
	"math"
	"sync"
)

// WeightFn returns the non-negative cost of traversing a rel (Dijkstra's
// assumption). It receives the step's source node and the RelRef, so it can
// read a stored weight via RelProp(rel.Pos, ...) or compute a derived cost;
// +Inf prunes the rel.
type WeightFn func(from NodeID, rel RelRef) float64

// ShortestPaths is the result of a Dijkstra search: the shortest distance
// to every reached node, with predecessors for path reconstruction. Small
// searches keep map state; a search that settles a large set switches to
// dense id-space arrays mid-run (see densify), so point queries stay
// cheap while component-scale searches avoid per-edge map probes.
type ShortestPaths struct {
	source NodeID
	// Sparse state (dense == false).
	dist map[NodeID]float64
	prev map[NodeID]NodeID
	// Dense state: distances indexed by id (+Inf = unreached), prev
	// indexed by id (prevNone = none), reached in settle-set order.
	dense   bool
	distA   []float64
	prevA   []NodeID
	reached []NodeID
	distMap map[NodeID]float64 // Distances() materialization, lazy
}

// prevNone marks a dense-mode node with no predecessor.
const prevNone = NodeID(^uint32(0))

// Distance is the shortest distance from the source to node; ok is false
// when unreached.
func (p *ShortestPaths) Distance(node NodeID) (float64, bool) {
	if p.dense {
		if int(node) >= len(p.distA) || math.IsInf(p.distA[node], 1) {
			return 0, false
		}
		return p.distA[node], true
	}
	d, ok := p.dist[node]
	return d, ok
}

// Reached reports whether node was reached from the source.
func (p *ShortestPaths) Reached(node NodeID) bool {
	_, ok := p.Distance(node)
	return ok
}

// Distances is every reached node with its shortest distance (the source
// itself at 0). The returned map is the result's own -- callers must not
// mutate it while still using PathTo.
func (p *ShortestPaths) Distances() map[NodeID]float64 {
	if !p.dense {
		return p.dist
	}
	if p.distMap == nil {
		p.distMap = make(map[NodeID]float64, len(p.reached))
		for _, v := range p.reached {
			p.distMap[v] = p.distA[v]
		}
	}
	return p.distMap
}

// PathTo is the shortest path from the source to target as a node sequence
// (source first); ok is false when target was unreached.
func (p *ShortestPaths) PathTo(target NodeID) ([]NodeID, bool) {
	if !p.Reached(target) {
		return nil, false
	}
	path := []NodeID{target}
	for cur := target; cur != p.source; {
		var next NodeID
		if p.dense {
			next = p.prevA[cur]
			if next == prevNone {
				return nil, false
			}
		} else {
			var ok bool
			next, ok = p.prev[cur]
			if !ok {
				return nil, false
			}
		}
		path = append(path, next)
		cur = next
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, true
}

// lookup is the mode-dispatched dist read used by the search loop.
func (p *ShortestPaths) lookup(node NodeID) (float64, bool) {
	if p.dense {
		d := p.distA[node]
		return d, !math.IsInf(d, 1)
	}
	d, ok := p.dist[node]
	return d, ok
}

// set is the mode-dispatched relax write.
func (p *ShortestPaths) set(node NodeID, d float64, from NodeID) {
	if p.dense {
		if math.IsInf(p.distA[node], 1) {
			p.reached = append(p.reached, node)
		}
		p.distA[node] = d
		p.prevA[node] = from
		return
	}
	p.dist[node] = d
	p.prev[node] = from
}

// densify moves a search that has settled a large set onto dense
// id-space arrays: one O(idspace) initialization buys per-edge relaxes
// with no hashing. Runs at most once per search.
func (p *ShortestPaths) densify(idSpace int) {
	p.distA = make([]float64, idSpace)
	p.prevA = make([]NodeID, idSpace)
	for i := range p.distA {
		p.distA[i] = math.Inf(1)
		p.prevA[i] = prevNone
	}
	p.reached = make([]NodeID, 0, 2*len(p.dist))
	for v, d := range p.dist {
		p.distA[v] = d
		p.reached = append(p.reached, v)
	}
	for v, u := range p.prev {
		p.prevA[v] = u
	}
	p.dense = true
	p.dist, p.prev = nil, nil
}

// dijkstraHeap is a min-heap ordered by cost, ties broken by node id (a
// total order, so runs are deterministic).
type dijkstraState struct {
	cost float64
	node NodeID
}

type dijkstraHeap []dijkstraState

// less orders by cost, ties by node id -- a total order for
// deterministic runs.
func (h dijkstraHeap) less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	return h[i].node < h[j].node
}

// push and pop are hand-rolled sift operations: the container/heap form
// boxes every state into an interface value, allocating once per push in
// the search's hottest loop.
func (h *dijkstraHeap) push(s dijkstraState) {
	*h = append(*h, s)
	a := *h
	for i := len(a) - 1; i > 0; {
		parent := (i - 1) / 2
		if !a.less(i, parent) {
			break
		}
		a[i], a[parent] = a[parent], a[i]
		i = parent
	}
}

func (h *dijkstraHeap) pop() dijkstraState {
	a := *h
	top := a[0]
	n := len(a) - 1
	a[0] = a[n]
	a = a[:n]
	*h = a
	for i := 0; ; {
		l := 2*i + 1
		if l >= n {
			break
		}
		small := l
		if r := l + 1; r < n && a.less(r, l) {
			small = r
		}
		if !a.less(small, i) {
			break
		}
		a[i], a[small] = a[small], a[i]
		i = small
	}
	return top
}

// Dijkstra runs weighted single-source shortest paths from source over the
// m-matched rels in dir, reaching the whole component.
func (g *Snapshot) Dijkstra(source NodeID, dir Direction, m RelMatch, weight WeightFn) *ShortestPaths {
	return g.dijkstra(source, dir, m, weight, false, 0)
}

// DijkstraTo is Dijkstra stopping as soon as target's shortest distance is
// known -- the single-pair form; the result still answers every node
// settled before the stop.
func (g *Snapshot) DijkstraTo(source, target NodeID, dir Direction, m RelMatch, weight WeightFn) *ShortestPaths {
	return g.dijkstra(source, dir, m, weight, true, target)
}

// dijkstraDenseAt is the settled-set size at which a search abandons its
// maps for dense id-space arrays -- big enough that point queries never
// pay the O(idspace) initialization, small enough that component-scale
// searches amortize it away.
const dijkstraDenseAt = 4096

func (g *Snapshot) dijkstra(source NodeID, dir Direction, m RelMatch, weight WeightFn,
	hasTarget bool, target NodeID) *ShortestPaths {
	p := &ShortestPaths{
		source: source,
		dist:   map[NodeID]float64{source: 0},
		prev:   map[NodeID]NodeID{},
	}
	h := dijkstraHeap{{cost: 0, node: source}}
	for len(h) > 0 {
		s := h.pop()
		if d, ok := p.lookup(s.node); ok && s.cost > d {
			continue // stale heap entry: a shorter path was found after pushing
		}
		if hasTarget && s.node == target {
			break
		}
		for rel := range g.RelsMatch(s.node, dir, m) {
			next := s.cost + weight(s.node, rel)
			if math.IsInf(next, 1) {
				continue // +Inf prunes the rel, per the WeightFn contract
			}
			if d, ok := p.lookup(rel.Neighbor); !ok || next < d {
				p.set(rel.Neighbor, next, s.node)
				h.push(dijkstraState{cost: next, node: rel.Neighbor})
			}
		}
		if !p.dense && len(p.dist) >= dijkstraDenseAt {
			p.densify(int(g.CSRIDSpace()))
		}
	}
	return p
}

// wspScratch is one bidirectional search's reusable state, pooled so a
// stage running many point-to-point searches (one per candidate pair)
// pays map and heap construction once per pool entry instead of per
// search -- clear keeps the maps' buckets, the heaps keep their backing.
type wspScratch struct {
	distF, distB map[NodeID]float64
	heapF, heapB dijkstraHeap
}

var wspPool = sync.Pool{New: func() any {
	return &wspScratch{distF: map[NodeID]float64{}, distB: map[NodeID]float64{}}
}}

// WeightedShortestPath is the shortest-path cost from source to target via
// bidirectional Dijkstra: it searches from both ends and meets in the
// middle, exploring far fewer nodes than a one-directional search for a
// point-to-point query. ok is false when target is unreachable. The
// backward search follows the reverse of dir, so weight must be symmetric
// (the usual case for an undirected Both traversal). For distances to many
// targets, or the path itself, use Dijkstra.
func (g *Snapshot) WeightedShortestPath(source, target NodeID, dir Direction, m RelMatch, weight WeightFn) (float64, bool) {
	if source == target {
		return 0, true
	}
	rev := dir.Reverse()
	scr := wspPool.Get().(*wspScratch)
	defer wspPool.Put(scr)
	clear(scr.distF)
	clear(scr.distB)
	distF, distB := scr.distF, scr.distB
	distF[source] = 0
	distB[target] = 0
	heapF := append(scr.heapF[:0], dijkstraState{cost: 0, node: source})
	heapB := append(scr.heapB[:0], dijkstraState{cost: 0, node: target})
	defer func() { scr.heapF, scr.heapB = heapF[:0], heapB[:0] }()
	// Best meeting cost so far; the frontiers cannot beat topF + topB.
	best := math.Inf(1)

	for len(heapF) > 0 || len(heapB) > 0 {
		topF, topB := math.Inf(1), math.Inf(1)
		if len(heapF) > 0 {
			topF = heapF[0].cost
		}
		if len(heapB) > 0 {
			topB = heapB[0].cost
		}
		if topF+topB >= best {
			break
		}
		// Expand whichever frontier is currently cheaper.
		h, dist, other, d := &heapF, distF, distB, dir
		if topF > topB {
			h, dist, other, d = &heapB, distB, distF, rev
		}
		if len(*h) == 0 {
			continue
		}
		s := h.pop()
		if cur, ok := dist[s.node]; ok && s.cost > cur {
			continue // stale
		}
		// s.node is settled on this side; if the other side reached it too,
		// the two halves form a candidate path.
		if otherCost, ok := other[s.node]; ok && s.cost+otherCost < best {
			best = s.cost + otherCost
		}
		for rel := range g.RelsMatch(s.node, d, m) {
			next := s.cost + weight(s.node, rel)
			if math.IsInf(next, 1) {
				continue // +Inf prunes the rel, per the WeightFn contract
			}
			if cur, ok := dist[rel.Neighbor]; !ok || next < cur {
				dist[rel.Neighbor] = next
				h.push(dijkstraState{cost: next, node: rel.Neighbor})
			}
		}
	}
	if !math.IsInf(best, 1) {
		return best, true
	}
	return 0, false
}
