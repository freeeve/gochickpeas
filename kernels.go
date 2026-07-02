// Neighborhood aggregation and co-occurrence kernels: forest roots of a
// functional rel chain (RootsVia/RootVia/NeighborVia), one-mode network
// folding (FoldVia), seeded co-occurrence (CoOccurring), and neighbor /
// common-neighbor histograms (NeighborCounts, CommonNeighbors,
// CommonNeighborCounts). NeighborGroups lives in neighborgroups.go.

package chickpeas

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/internal/parallel"
	"github.com/freeeve/gochickpeas/nodeset"
)

// RootsVia is a built forest-root array: index it by node id for the
// terminal of that node's functional-relationship chain. Shared and
// immutable -- hot loops hold it and index lock-free.
type RootsVia []NodeID

// NoNeighbor is the sentinel a NeighborVia array holds for a node with no
// such neighbor.
const NoNeighbor = NodeID(^uint32(0))

type rootsKey struct {
	dir Direction
	t   RelType
}

// RootsVia is the per-node forest-root array for the functional relType
// chain in dir: roots[node] is the node reached by following the single
// relType rel until one with no such rel (a terminal node maps to itself).
// Built once per (direction, type) with path compression, then cached --
// call once and index the slice, rather than RootVia per node. Intended
// for a rel that is functional in dir; malformed data (multiple such rels,
// or a cycle) follows the first neighbor in CSR order and is broken by a
// depth cap, resolving deterministically.
func (g *Snapshot) RootsVia(t RelType, dir Direction) RootsVia {
	key := rootsKey{dir: dir, t: t}
	g.rootsMu.Lock()
	cached, ok := g.rootsViaIndex[key]
	g.rootsMu.Unlock()
	if ok {
		return cached
	}
	// Build outside the lock; a racing build is discarded (identical).
	built := g.buildRootsVia(dir, t)
	g.rootsMu.Lock()
	defer g.rootsMu.Unlock()
	if existing, ok := g.rootsViaIndex[key]; ok {
		return existing
	}
	g.rootsViaIndex[key] = built
	return built
}

// RootVia is the terminal of node's functional relType chain in dir (a
// terminal node maps to itself) -- a convenience over RootsVia; in a hot
// loop index the array instead.
func (g *Snapshot) RootVia(node NodeID, t RelType, dir Direction) NodeID {
	roots := g.RootsVia(t, dir)
	if int(node) < len(roots) {
		return roots[node]
	}
	return node
}

// NeighborVia is the single neighbor each node reaches via the functional
// relType in dir (one hop -- e.g. a message's hasCreator); the depth-1
// sibling of RootsVia. Node-indexed; a node with no such neighbor maps to
// NoNeighbor. Built fresh, not cached.
func (g *Snapshot) NeighborVia(t RelType, dir Direction) RootsVia {
	n := int(g.CSRIDSpace())
	m := MatchType(t)
	out := make(RootsVia, n)
	for node := range NodeID(n) {
		out[node] = NoNeighbor
		for nb := range g.NeighborsMatch(node, dir, m) {
			out[node] = nb
			break
		}
	}
	return out
}

// buildRootsVia resolves every node's chain terminal once via path
// compression; a depth cap of the id-space size breaks any malformed
// cycle, so the build always terminates deterministically.
func (g *Snapshot) buildRootsVia(dir Direction, t RelType) RootsVia {
	n := int(g.CSRIDSpace())
	m := MatchType(t)
	const unresolved = NodeID(^uint32(0))
	root := make(RootsVia, n)
	for i := range root {
		root[i] = unresolved
	}
	var path []NodeID
	for start := range NodeID(n) {
		if root[start] != unresolved {
			continue
		}
		path = path[:0]
		cur := start
		var terminal NodeID
		for {
			if resolved := root[cur]; resolved != unresolved {
				terminal = resolved
				break
			}
			next, ok := g.FirstNeighborMatch(cur, dir, m)
			// path length <= n bounds a malformed cycle (a valid chain has
			// at most n-1 rels, so this never trips on a forest).
			if !ok || len(path) > n {
				terminal = cur
				break
			}
			path = append(path, cur)
			cur = next
		}
		for _, node := range path {
			root[node] = terminal
		}
		root[start] = terminal
	}
	return root
}

// FirstNeighborMatch is FirstNeighbor over a pre-resolved RelMatch.
func (g *Snapshot) FirstNeighborMatch(node NodeID, dir Direction, m RelMatch) (NodeID, bool) {
	for n := range g.NeighborsMatch(node, dir, m) {
		return n, true
	}
	return 0, false
}

// NodePair is an unordered node pair (Lo <= Hi), the key of FoldVia.
type NodePair struct {
	Lo, Hi NodeID
}

// FoldVia folds the m-matched rels (in dir) into a weighted node-pair map
// by projecting both endpoints of each rel through projection -- the
// one-mode / bipartite projection ("network folding") of a relation onto a
// derived node set. For every matched rel a -> b, a' = projection[a] and
// b' = projection[b] add one to the unordered pair count; self-pairs and
// endpoints projecting to NoNeighbor are skipped. projection is a flat
// node -> node array (a NeighborVia or RootsVia). Runs in parallel over
// the id space, per-chunk maps merged small-into-large.
func (g *Snapshot) FoldVia(m RelMatch, dir Direction, projection []NodeID) map[NodePair]uint64 {
	n := int(g.CSRIDSpace())
	return parallel.Fold(n,
		func() map[NodePair]uint64 { return map[NodePair]uint64{} },
		func(acc map[NodePair]uint64, i int) map[NodePair]uint64 {
			if i >= len(projection) {
				return acc
			}
			a := projection[i]
			if a == NoNeighbor {
				return acc
			}
			for dst := range g.NeighborsMatch(NodeID(i), dir, m) {
				if int(dst) >= len(projection) {
					continue
				}
				b := projection[dst]
				if b == NoNeighbor || a == b {
					continue
				}
				pair := NodePair{Lo: min(a, b), Hi: max(a, b)}
				acc[pair]++
			}
			return acc
		},
		func(x, y map[NodePair]uint64) map[NodePair]uint64 {
			if len(x) < len(y) {
				x, y = y, x
			}
			for k, v := range y {
				x[k] += v
			}
			return x
		})
}

// CoWeight selects how CoOccurring accumulates each co-occurring node's
// weight (declarative -- the kernel needs no per-element callback).
type CoWeight struct {
	distinct    bool
	distinctKey string
}

// CoCount weights by the number of shared centers (co-occurrence events).
func CoCount() CoWeight {
	return CoWeight{}
}

// CoDistinct weights by the number of distinct values of the property key
// read off each shared center (e.g. distinct co-occurrence days); a center
// lacking the key contributes nothing.
func CoDistinct(key string) CoWeight {
	return CoWeight{distinct: true, distinctKey: key}
}

// CoOccurring is seeded co-occurrence -- one-mode projection by shared
// neighbor: from seed over the m-matched rels, the nodes sharing a
// neighbor with seed (seed -(m,dir)-> centers -(m,reversed)-> others),
// seed itself excluded, each weighted per w. The seeded row of the
// node-node co-occurrence matrix; the by-shared-neighbor complement of
// FoldVia's by-rel-endpoint projection.
func (g *Snapshot) CoOccurring(seed NodeID, m RelMatch, dir Direction, w CoWeight) map[NodeID]uint64 {
	back := dir.Reverse()
	if !w.distinct {
		counts := map[NodeID]uint64{}
		for center := range g.NeighborsMatch(seed, dir, m) {
			for other := range g.NeighborsMatch(center, back, m) {
				if other != seed {
					counts[other]++
				}
			}
		}
		return counts
	}
	keyID, ok := g.PropertyKey(w.distinctKey)
	if !ok {
		return map[NodeID]uint64{}
	}
	column, ok := g.columns[keyID]
	if !ok {
		return map[NodeID]uint64{}
	}
	sets := map[NodeID]map[Value]struct{}{}
	for center := range g.NeighborsMatch(seed, dir, m) {
		val, ok := column.Get(center)
		if !ok {
			continue
		}
		for other := range g.NeighborsMatch(center, back, m) {
			if other == seed {
				continue
			}
			s, ok := sets[other]
			if !ok {
				s = map[Value]struct{}{}
				sets[other] = s
			}
			s[val] = struct{}{}
		}
	}
	out := make(map[NodeID]uint64, len(sets))
	for other, s := range sets {
		out[other] = uint64(len(s))
	}
	return out
}

// NeighborCounts is the histogram of neighbor nodes reached from sources
// via the m-matched rels in dir (how many of the sources point to each
// neighbor). Counts into pooled generation-stamped dense scratch.
func (g *Snapshot) NeighborCounts(sources []NodeID, dir Direction, m RelMatch) map[NodeID]int {
	n := int(g.CSRIDSpace())
	s := takeScratch(n)
	defer scratchPool.Put(s)
	var touched []NodeID
	for _, src := range sources {
		for t := range g.NeighborsMatch(src, dir, m) {
			if s.gen[t] != s.cur {
				s.gen[t] = s.cur
				s.dist[t] = 0
				touched = append(touched, t)
			}
			s.dist[t]++
		}
	}
	counts := make(map[NodeID]int, len(touched))
	for _, t := range touched {
		counts[t] = int(s.dist[t])
	}
	return counts
}

// CommonNeighbors is N(a) ∩ N(b) over the m-matched rels in dir (Both
// gives the undirected common neighborhood) -- the link-prediction
// primitive, returned as a set so it composes with And/Or/AndNot.
func (g *Snapshot) CommonNeighbors(a, b NodeID, dir Direction, m RelMatch) *nodeset.Set {
	na, nb := roaring.New(), roaring.New()
	for x := range g.NeighborsMatch(a, dir, m) {
		na.Add(x)
	}
	for x := range g.NeighborsMatch(b, dir, m) {
		nb.Add(x)
	}
	return nodeset.FromBitmap(roaring.And(na, nb))
}

// CommonNeighborCount is one (source, target, count) triple of
// CommonNeighborCounts.
type CommonNeighborCount struct {
	Source, Target NodeID
	Count          uint64
}

// pairScratch is the per-worker scratch behind CommonNeighborCounts: gen-
// stamped counts plus two dedup stamps (per-source mids, per-mid targets).
type pairScratch struct {
	val          []uint64
	gen, seenMid []uint32
	tSeen        []uint32
	cur, tCur    uint32
	touched      []NodeID
}

var pairScratchPool = sync.Pool{New: func() any { return &pairScratch{} }}

// CommonNeighborCounts is the masked A^2 / link-prediction primitive: for
// each s in sources, count the DISTINCT mid nodes on two-hop paths
// s -[m]- mid -[m]- t whose endpoint t is in targets, emitting one
// (s, t, count) per reached pair (count > 0 only). count is |N(s) ∩ N(t)|
// over de-duplicated neighborhoods -- exact even when the rel is stored
// bidirectionally and traversed Both -- not a walk multiplicity. (s, s)
// self-pairs ARE emitted; callers wanting distinct endpoints filter them.
// Sources are processed in parallel (chunk results concatenated in source
// order, so output order is deterministic); counting scatters from each
// source through its mids (a "push"), far cheaper than per-pair set
// intersections when few pairs share a neighbor.
func (g *Snapshot) CommonNeighborCounts(sources []NodeID, dir Direction, m RelMatch, targets *nodeset.Set) []CommonNeighborCount {
	n := int(g.CSRIDSpace())
	perChunk := parallel.Fold(len(sources),
		func() []CommonNeighborCount { return nil },
		func(acc []CommonNeighborCount, i int) []CommonNeighborCount {
			s := sources[i]
			sc := pairScratchPool.Get().(*pairScratch)
			defer pairScratchPool.Put(sc)
			if len(sc.val) < n {
				sc.val = make([]uint64, n)
				sc.gen = make([]uint32, n)
				sc.seenMid = make([]uint32, n)
				sc.tSeen = make([]uint32, n)
				sc.cur, sc.tCur = 0, 0
			}
			sc.cur++
			if sc.cur == 0 {
				clear(sc.gen)
				clear(sc.seenMid)
				sc.cur = 1
			}
			cur := sc.cur
			sc.touched = sc.touched[:0]
			for mid := range g.NeighborsMatch(s, dir, m) {
				// First-hop dedup: process each distinct mid once.
				if sc.seenMid[mid] == cur {
					continue
				}
				sc.seenMid[mid] = cur
				sc.tCur++
				if sc.tCur == 0 {
					clear(sc.tSeen)
					sc.tCur = 1
				}
				tCur := sc.tCur
				for t := range g.NeighborsMatch(mid, dir, m) {
					if !targets.Contains(t) {
						continue
					}
					// Second-hop dedup: bump each distinct endpoint once per mid.
					if sc.tSeen[t] == tCur {
						continue
					}
					sc.tSeen[t] = tCur
					if sc.gen[t] != cur {
						sc.gen[t] = cur
						sc.val[t] = 0
						sc.touched = append(sc.touched, t)
					}
					sc.val[t]++
				}
			}
			for _, t := range sc.touched {
				acc = append(acc, CommonNeighborCount{Source: s, Target: t, Count: sc.val[t]})
			}
			return acc
		},
		func(a, b []CommonNeighborCount) []CommonNeighborCount { return append(a, b...) })
	return perChunk
}
