// Neighbor-counting and common-neighbor analytics: the neighbor histogram,
// the pairwise common-neighborhood set, and the masked A^2 / link-
// prediction primitive (per-source distinct two-hop endpoint counts,
// folded in parallel). Split from kernels.go, which holds the
// roots-via / functional-relationship kernels.
package chickpeas

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/nodeset"
	"github.com/freeeve/gochickpeas/parallel"
)

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
// cncChunk is the pair accumulator's slab size: fixed chunks replace the
// per-worker doubling ladder (each doubling discarded its predecessor,
// dominating the primitive's allocation profile), and the final assembly
// is one exact-size copy.
const cncChunk = 4096

// cncAcc is one worker's accumulation state: the worker-held scratch
// (one pool round-trip per worker, not per source) and the ordered pair
// slabs.
type cncAcc struct {
	sc   *pairScratch
	full [][]CommonNeighborCount
	cur  []CommonNeighborCount
}

// seal closes the open slab into the ordered list.
func (a *cncAcc) seal() {
	if len(a.cur) > 0 {
		a.full = append(a.full, a.cur)
		a.cur = nil
	}
}

func (g *Snapshot) CommonNeighborCounts(sources []NodeID, dir Direction, m RelMatch, targets *nodeset.Set) []CommonNeighborCount {
	n := int(g.CSRIDSpace())
	acc := parallel.Fold(len(sources),
		func() *cncAcc { return &cncAcc{sc: pairScratchPool.Get().(*pairScratch)} },
		func(acc *cncAcc, i int) *cncAcc {
			s := sources[i]
			sc := acc.sc
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
				if len(acc.cur) == cncChunk {
					acc.full = append(acc.full, acc.cur)
					acc.cur = make([]CommonNeighborCount, 0, cncChunk)
				}
				if acc.cur == nil {
					acc.cur = make([]CommonNeighborCount, 0, cncChunk)
				}
				acc.cur = append(acc.cur, CommonNeighborCount{Source: s, Target: t, Count: sc.val[t]})
			}
			return acc
		},
		// Merge keeps slab order (ascending worker ranges): the left side's
		// open slab seals before the right side's slabs append, and the
		// right side's scratch returns to the pool.
		func(a, b *cncAcc) *cncAcc {
			a.seal()
			b.seal()
			a.full = append(a.full, b.full...)
			if b.sc != nil {
				pairScratchPool.Put(b.sc)
				b.sc = nil
			}
			return a
		})
	acc.seal()
	if acc.sc != nil {
		pairScratchPool.Put(acc.sc)
		acc.sc = nil
	}
	total := 0
	for _, ch := range acc.full {
		total += len(ch)
	}
	out := make([]CommonNeighborCount, 0, total)
	for _, ch := range acc.full {
		out = append(out, ch...)
	}
	return out
}
