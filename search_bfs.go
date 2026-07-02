// Unweighted traversal search: reachability, (bidirectional) BFS over node
// sets with nil-able node/rel filters, hop distances, and bounded
// neighborhoods. The distance kernels reuse generation-stamped dense
// scratch from a pool, so repeated searches on a large graph avoid an O(n)
// clear (and allocation) per call.

package chickpeas

import (
	"sync"

	"github.com/freeeve/gochickpeas/nodeset"
)

// NoMaxDepth removes the depth bound of a search.
const NoMaxDepth = -1

// NodeFilter gates a node's participation in a search; nil means no filter.
type NodeFilter func(node NodeID, g *Snapshot) bool

// RelFilter gates following a rel, receiving its stored (source, target)
// endpoints, type, and CSR position; nil means no filter.
type RelFilter func(from, to NodeID, t RelType, pos uint32, g *Snapshot) bool

// CanReach reports whether to is reachable from from over the m-matched
// rels in dir, within maxDepth hops (NoMaxDepth = unbounded).
func (g *Snapshot) CanReach(from, to NodeID, dir Direction, m RelMatch, maxDepth int) bool {
	if from == to {
		return true
	}
	visited := nodeset.Of(from)
	queue := [][2]uint32{{from, 0}}
	for len(queue) > 0 {
		current, depth := queue[0][0], queue[0][1]
		queue = queue[1:]
		if maxDepth >= 0 && int(depth) >= maxDepth {
			continue
		}
		for neighbor := range g.NeighborsMatch(current, dir, m) {
			if neighbor == to {
				return true
			}
			if visited.Insert(neighbor) {
				queue = append(queue, [2]uint32{neighbor, depth + 1})
			}
		}
	}
	return false
}

// bfsStep expands one node of a BFS frontier, shared by BFS and both passes
// of BidirectionalBFS. backward flips the (from, to) the rel filter sees.
func (g *Snapshot) bfsStep(current NodeID, depth uint32, dir Direction, m RelMatch,
	nodeFilter NodeFilter, relFilter RelFilter, backward bool,
	visitedNodes, visitedRels, otherVisited *nodeset.Set, queue *[][2]uint32) {
	for rel := range g.RelsMatch(current, dir, m) {
		// The rel filter sees traversal orientation -- (current, neighbor)
		// on the forward pass, flipped on the backward pass -- matching the
		// Rust kernel.
		from, to := current, rel.Neighbor
		if backward {
			from, to = rel.Neighbor, current
		}
		if relFilter != nil && !relFilter(from, to, rel.Type, rel.Pos, g) {
			continue
		}
		// The node filter applies before recording/marking, so a rejected
		// node never participates regardless of which frontier reaches it.
		if nodeFilter != nil && !nodeFilter(rel.Neighbor, g) {
			continue
		}
		if otherVisited != nil && otherVisited.Contains(rel.Neighbor) {
			// Meeting point: always record the connecting rel; enqueue only
			// if newly visited.
			visitedRels.Insert(rel.Pos)
			if visitedNodes.Insert(rel.Neighbor) {
				*queue = append(*queue, [2]uint32{rel.Neighbor, depth + 1})
			}
			continue
		}
		if otherVisited != nil {
			if visitedNodes.Insert(rel.Neighbor) {
				visitedRels.Insert(rel.Pos)
				*queue = append(*queue, [2]uint32{rel.Neighbor, depth + 1})
			}
			continue
		}
		visitedRels.Insert(rel.Pos)
		if visitedNodes.Insert(rel.Neighbor) {
			*queue = append(*queue, [2]uint32{rel.Neighbor, depth + 1})
		}
	}
}

// BFS traverses from the start set over the m-matched rels in dir,
// returning every node visited and every rel CSR position traversed.
// Filters gate participation (nil = none); maxDepth bounds the hop count
// (NoMaxDepth = unbounded).
func (g *Snapshot) BFS(start *nodeset.Set, dir Direction, m RelMatch,
	nodeFilter NodeFilter, relFilter RelFilter, maxDepth int) (nodes, rels *nodeset.Set) {
	nodes, rels = nodeset.New(), nodeset.New()
	var queue [][2]uint32
	for id := range start.Iter() {
		if (nodeFilter == nil || nodeFilter(id, g)) && nodes.Insert(id) {
			queue = append(queue, [2]uint32{id, 0})
		}
	}
	for len(queue) > 0 {
		current, depth := queue[0][0], queue[0][1]
		queue = queue[1:]
		if maxDepth >= 0 && int(depth) >= maxDepth {
			continue
		}
		g.bfsStep(current, depth, dir, m, nodeFilter, relFilter, false, nodes, rels, nil, &queue)
	}
	return nodes, rels
}

// BidirectionalBFS searches from the source and target sets
// simultaneously, meeting in the middle: the forward pass follows dir, the
// backward pass its reverse. Returns the nodes reached by BOTH sides (the
// meeting set) and the union of rel positions either side traversed; both
// empty when the sets don't connect. An immediate source/target overlap
// returns it with no rels.
func (g *Snapshot) BidirectionalBFS(source, target *nodeset.Set, dir Direction, m RelMatch,
	nodeFilter NodeFilter, relFilter RelFilter, maxDepth int) (nodes, rels *nodeset.Set) {
	fNodes, fRels := nodeset.New(), nodeset.New()
	bNodes, bRels := nodeset.New(), nodeset.New()
	var fQueue, bQueue [][2]uint32
	for id := range source.Iter() {
		if nodeFilter == nil || nodeFilter(id, g) {
			fNodes.Insert(id)
			fQueue = append(fQueue, [2]uint32{id, 0})
		}
	}
	for id := range target.Iter() {
		if nodeFilter == nil || nodeFilter(id, g) {
			bNodes.Insert(id)
			bQueue = append(bQueue, [2]uint32{id, 0})
		}
	}
	if overlap := fNodes.And(bNodes); !overlap.IsEmpty() {
		return overlap, nodeset.New()
	}
	rev := dir.Reverse()

	expand := func(queue *[][2]uint32, visited, visitedRels, other *nodeset.Set, d Direction, backward bool) {
		for range len(*queue) { // one level
			current, depth := (*queue)[0][0], (*queue)[0][1]
			*queue = (*queue)[1:]
			if maxDepth >= 0 && int(depth) >= maxDepth {
				continue
			}
			g.bfsStep(current, depth, d, m, nodeFilter, relFilter, backward, visited, visitedRels, other, queue)
		}
	}
	for len(fQueue) > 0 || len(bQueue) > 0 {
		expand(&fQueue, fNodes, fRels, bNodes, dir, false)
		expand(&bQueue, bNodes, bRels, fNodes, rev, true)
	}

	meeting := fNodes.And(bNodes)
	if meeting.IsEmpty() {
		return nodeset.New(), nodeset.New()
	}
	// The rel union covers everything both sides visited, not only rels
	// between meeting nodes (filtering those would need path
	// reconstruction) -- same contract as the Rust kernel.
	return meeting, fRels.Or(bRels)
}

// searchScratch is the generation-stamped dense visited/distance scratch
// behind BFSDistances and Neighborhood: a node counts as visited this run
// iff gen[node] == cur, avoiding an O(n) clear per call. Pooled on the
// snapshot so concurrent searches each take their own.
type searchScratch struct {
	gen  []uint32
	dist []uint32
	cur  uint32
}

var scratchPool = sync.Pool{New: func() any { return &searchScratch{} }}

func takeScratch(n int) *searchScratch {
	s := scratchPool.Get().(*searchScratch)
	if len(s.gen) < n {
		s.gen = make([]uint32, n)
		s.dist = make([]uint32, n)
		s.cur = 0
	}
	s.cur++
	if s.cur == 0 { // wrapped: reset the stamps once
		clear(s.gen)
		s.cur = 1
	}
	return s
}

// BFSDistances is unweighted single-source BFS returning the hop distance
// from start to every reached node (start itself at 0), bounded by
// maxDepth (NoMaxDepth = the whole component). Cheaper than Dijkstra with
// a unit weight when only hop counts are needed.
func (g *Snapshot) BFSDistances(start NodeID, dir Direction, m RelMatch, maxDepth int) map[NodeID]uint32 {
	n := int(g.CSRIDSpace())
	// A start outside the CSR id space is not a node in this graph.
	if int(start) >= n {
		return map[NodeID]uint32{}
	}
	s := takeScratch(n)
	defer scratchPool.Put(s)

	s.gen[start] = s.cur
	s.dist[start] = 0
	touched := []NodeID{start}
	frontier := []NodeID{start}
	depth := uint32(0)
	for len(frontier) > 0 {
		if maxDepth >= 0 && int(depth) >= maxDepth {
			break
		}
		depth++
		var next []NodeID
		for _, node := range frontier {
			for neighbor := range g.NeighborsMatch(node, dir, m) {
				if s.gen[neighbor] != s.cur {
					s.gen[neighbor] = s.cur
					s.dist[neighbor] = depth
					next = append(next, neighbor)
					touched = append(touched, neighbor)
				}
			}
		}
		frontier = next
	}
	out := make(map[NodeID]uint32, len(touched))
	for _, node := range touched {
		out[node] = s.dist[node]
	}
	return out
}

// Neighborhood is the set of nodes whose hop distance from seed lies in
// the closed range [loHops, hiHops] -- 1..2 is "one or two hops out"
// (excludes seed), 0..2 includes it, 2..2 is exactly two hops. Returned as
// a set so membership is O(1) and intersecting with another set is one
// bitmap op.
func (g *Snapshot) Neighborhood(seed NodeID, dir Direction, m RelMatch, loHops, hiHops uint32) *nodeset.Set {
	result := nodeset.New()
	n := int(g.CSRIDSpace())
	if loHops > hiHops || int(seed) >= n {
		return result
	}
	s := takeScratch(n)
	defer scratchPool.Put(s)

	s.gen[seed] = s.cur
	if loHops == 0 {
		result.Insert(seed)
	}
	frontier := []NodeID{seed}
	for depth := uint32(1); depth <= hiHops && len(frontier) > 0; depth++ {
		var next []NodeID
		for _, node := range frontier {
			for neighbor := range g.NeighborsMatch(node, dir, m) {
				if s.gen[neighbor] != s.cur {
					s.gen[neighbor] = s.cur
					next = append(next, neighbor)
					if depth >= loHops {
						result.Insert(neighbor)
					}
				}
			}
		}
		frontier = next
	}
	return result
}
