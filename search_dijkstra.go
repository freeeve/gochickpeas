// Weighted shortest-path search: single-source Dijkstra with path
// reconstruction, and the bidirectional point-to-point variant that meets
// in the middle.

package chickpeas

import (
	"container/heap"
	"math"
)

// WeightFn returns the non-negative cost of traversing a rel (Dijkstra's
// assumption). It receives the step's source node and the RelRef, so it can
// read a stored weight via RelProp(rel.Pos, ...) or compute a derived cost;
// +Inf prunes the rel.
type WeightFn func(from NodeID, rel RelRef) float64

// ShortestPaths is the result of a Dijkstra search: the shortest distance
// to every reached node, with predecessors for path reconstruction.
type ShortestPaths struct {
	source NodeID
	dist   map[NodeID]float64
	prev   map[NodeID]NodeID
}

// Distance is the shortest distance from the source to node; ok is false
// when unreached.
func (p *ShortestPaths) Distance(node NodeID) (float64, bool) {
	d, ok := p.dist[node]
	return d, ok
}

// Reached reports whether node was reached from the source.
func (p *ShortestPaths) Reached(node NodeID) bool {
	_, ok := p.dist[node]
	return ok
}

// Distances is every reached node with its shortest distance (the source
// itself at 0). The returned map is the result's own -- callers must not
// mutate it while still using PathTo.
func (p *ShortestPaths) Distances() map[NodeID]float64 {
	return p.dist
}

// PathTo is the shortest path from the source to target as a node sequence
// (source first); ok is false when target was unreached.
func (p *ShortestPaths) PathTo(target NodeID) ([]NodeID, bool) {
	if _, ok := p.dist[target]; !ok {
		return nil, false
	}
	path := []NodeID{target}
	for cur := target; cur != p.source; {
		next, ok := p.prev[cur]
		if !ok {
			return nil, false
		}
		path = append(path, next)
		cur = next
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, true
}

// dijkstraHeap is a min-heap ordered by cost, ties broken by node id (a
// total order, so runs are deterministic).
type dijkstraState struct {
	cost float64
	node NodeID
}

type dijkstraHeap []dijkstraState

func (h dijkstraHeap) Len() int { return len(h) }
func (h dijkstraHeap) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	return h[i].node < h[j].node
}
func (h dijkstraHeap) Swap(i, j int)         { h[i], h[j] = h[j], h[i] }
func (h *dijkstraHeap) Push(x any)           { *h = append(*h, x.(dijkstraState)) }
func (h *dijkstraHeap) Pop() any             { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
func (h *dijkstraHeap) push(s dijkstraState) { heap.Push(h, s) }
func (h *dijkstraHeap) pop() dijkstraState   { return heap.Pop(h).(dijkstraState) }

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

func (g *Snapshot) dijkstra(source NodeID, dir Direction, m RelMatch, weight WeightFn,
	hasTarget bool, target NodeID) *ShortestPaths {
	dist := map[NodeID]float64{source: 0}
	prev := map[NodeID]NodeID{}
	h := dijkstraHeap{{cost: 0, node: source}}
	for len(h) > 0 {
		s := h.pop()
		if d, ok := dist[s.node]; ok && s.cost > d {
			continue // stale heap entry: a shorter path was found after pushing
		}
		if hasTarget && s.node == target {
			break
		}
		for rel := range g.RelsMatch(s.node, dir, m) {
			next := s.cost + weight(s.node, rel)
			if d, ok := dist[rel.Neighbor]; !ok || next < d {
				dist[rel.Neighbor] = next
				prev[rel.Neighbor] = s.node
				h.push(dijkstraState{cost: next, node: rel.Neighbor})
			}
		}
	}
	return &ShortestPaths{source: source, dist: dist, prev: prev}
}

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
	distF := map[NodeID]float64{source: 0}
	distB := map[NodeID]float64{target: 0}
	heapF := dijkstraHeap{{cost: 0, node: source}}
	heapB := dijkstraHeap{{cost: 0, node: target}}
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
