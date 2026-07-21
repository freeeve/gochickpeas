// The BI weighted-shortest-path weight derivations (Q15/Q19/Q20 + IC14),
// shared by the native kernels (which consume the maps directly) and by
// cmd/weightedexport (task 049), which materializes them as relationship
// types with a `w` property so the GQL COST clause has a graph to run on
// -- mirroring the rcp side's weight tables (their python/cypher/
// weights.py).
package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/parallel"
)

// weightMap is a flat undirected-pair -> float64 accumulator: a probe
// index over packed (lo<<32|hi) keys plus a parallel weight slab -- the
// map[interactionKey]float64 form paid bucket growth per fold shard.
type weightMap struct {
	idx flatset.U64Map
	w   []float64
}

// pairKey64 packs the undirected pair as lo<<32|hi.
func pairKey64(a, b chickpeas.NodeID) uint64 {
	if a > b {
		a, b = b, a
	}
	return uint64(uint32(a))<<32 | uint64(uint32(b))
}

// addAt accumulates v into the pair's weight, minting the slot on first
// sight.
func (m *weightMap) addAt(key uint64, v float64) {
	i := m.idx.GetOrCreate(key, func() int {
		m.w = append(m.w, 0)
		return len(m.w) - 1
	})
	m.w[i] += v
}

// get returns the pair's accumulated weight; ok is false when the pair
// never scored.
func (m *weightMap) get(key uint64) (float64, bool) {
	i, ok := m.idx.Get(key)
	if !ok {
		return 0, false
	}
	return m.w[i], true
}

// reset empties the map keeping index table and slab backings.
func (m *weightMap) reset() {
	m.idx.Reset()
	m.w = m.w[:0]
}

// newWeightAccs seeds one weightMap per worker for FoldInto.
func newWeightAccs() []*weightMap {
	accs := make([]*weightMap, parallel.Workers())
	for i := range accs {
		accs[i] = &weightMap{}
	}
	return accs
}

// q15WeightMap is Q15's discounted reply-interaction weights: replies
// whose thread-root forum was created in the Nov-2010 window score their
// creator pair (a Post parent 1.0, a Comment parent 0.5), keyed by
// undirected pair. The traversal weight is 1/(score+1). accs supplies the
// borrowed per-worker accumulators (reset here), so a repeat caller's
// warm build allocates nothing for accumulator state.
func q15WeightMap(g *chickpeas.Snapshot, accs []*weightMap) (*weightMap, error) {
	startDay, endDay := dayFromCivil(2010, 11, 1), dayFromCivil(2010, 12, 1)
	fdayCol, err := nodeI64Col(g, "fday")
	if err != nil {
		return nil, err
	}
	for _, a := range accs {
		a.reset()
	}
	posts, _ := g.NodesWithLabel("Post")
	comments, ok := g.NodesWithLabel("Comment")
	replyOf, okRt := g.RelType("REPLY_OF")
	if !ok || !okRt {
		return &weightMap{}, nil
	}
	roots := g.RootsVia(replyOf, chickpeas.Outgoing)
	ids := comments.ToSlice()
	return parallel.FoldInto(accs, len(ids),
		func(acc *weightMap, i int) *weightMap {
			c := ids[i]
			parent, ok := g.FirstNeighbor(c, chickpeas.Outgoing, "REPLY_OF")
			if !ok {
				return acc
			}
			cc, ok1 := creatorOf(g, c)
			pc, ok2 := creatorOf(g, parent)
			if !ok1 || !ok2 || cc == pc {
				return acc
			}
			root := roots[c]
			forum, ok := g.FirstNeighbor(root, chickpeas.Incoming, "CONTAINER_OF")
			if !ok {
				return acc
			}
			fday := i64At(fdayCol, forum)
			if fday >= startDay && fday <= endDay {
				contrib := 0.5
				if posts != nil && posts.Contains(parent) {
					contrib = 1.0
				}
				acc.addAt(pairKey64(cc, pc), contrib)
			}
			return acc
		},
		func(a, b *weightMap) *weightMap {
			b.idx.ForEach(func(key uint64, bi int) {
				a.addAt(key, b.w[bi])
			})
			return a
		}), nil
}

// cohortWeightMap is Q20's cohort weights: for each knows pair sharing a
// university, min |classYear difference| + 1 over the shared enrolments.
func cohortWeightMap(g *chickpeas.Snapshot) (map[interactionKey]float64, error) {
	cyCol, ok := g.RelColIndexed("classYear")
	if !ok {
		return nil, fmt.Errorf("rel column classYear missing")
	}
	cy := cyCol.I64()
	studyat := map[chickpeas.NodeID][]studyRecord{}
	if persons, ok := g.NodesWithLabel("Person"); ok {
		for p := range persons.Iter() {
			var recs []studyRecord
			for r := range g.Rels(p, chickpeas.Outgoing, "STUDY_AT") {
				year, _ := cy.Get(r.Pos)
				recs = append(recs, studyRecord{r.Neighbor, year})
			}
			if len(recs) > 0 {
				studyat[p] = recs
			}
		}
	}
	wm := map[interactionKey]float64{}
	for a, sa := range studyat {
		for b := range g.Neighbors(a, chickpeas.Both, "KNOWS") {
			if b <= a {
				continue
			}
			sb, ok := studyat[b]
			if !ok {
				continue
			}
			best := int64(-1)
			for _, ra := range sa {
				for _, rb := range sb {
					if ra.uni != rb.uni {
						continue
					}
					d := ra.year - rb.year
					if d < 0 {
						d = -d
					}
					if best < 0 || d < best {
						best = d
					}
				}
			}
			if best >= 0 {
				wm[interactionKey{a, b}] = float64(best + 1)
			}
		}
	}
	return wm, nil
}

// WeightEdge is one derived weighted relationship to materialize.
type WeightEdge struct {
	From, To chickpeas.NodeID
	W        float64
}

// DeriveWeightRels computes the materialized weight relations over the
// knows graph, both directions per undirected pair (knows is undirected),
// mirroring the rcp weight tables exactly:
//
//   - q15weight: every knows pair, 1/(forum-window reply score + 1)
//   - interactsWith: pairs that interacted (replies, either direction),
//     1/interactions
//   - cohort: pairs sharing a university, min |classYear diff| + 1
//   - ic14weight: every knows pair, 1/(interactions + 1)
func DeriveWeightRels(g *chickpeas.Snapshot) (map[string][]WeightEdge, error) {
	persons, ok := g.NodesWithLabel("Person")
	if !ok {
		return nil, fmt.Errorf("no Person label")
	}
	pairSet := map[interactionKey]struct{}{}
	for a := range persons.Iter() {
		for b := range g.Neighbors(a, chickpeas.Both, "KNOWS") {
			if a < b {
				pairSet[interactionKey{a, b}] = struct{}{}
			}
		}
	}
	pairs := make([]interactionKey, 0, len(pairSet))
	for k := range pairSet {
		pairs = append(pairs, k)
	}
	sortByLess(pairs, func(a, b interactionKey) bool {
		if a.lo != b.lo {
			return a.lo < b.lo
		}
		return a.hi < b.hi
	})

	inter := buildInteractionMap(g)
	q15w, err := q15WeightMap(g, newWeightAccs())
	if err != nil {
		return nil, err
	}
	cohortW, err := cohortWeightMap(g)
	if err != nil {
		return nil, err
	}

	out := map[string][]WeightEdge{}
	both := func(typ string, k interactionKey, w float64) {
		out[typ] = append(out[typ], WeightEdge{k.lo, k.hi, w}, WeightEdge{k.hi, k.lo, w})
	}
	for _, k := range pairs {
		q15v, _ := q15w.get(pairKey64(k.lo, k.hi)) // absent pair scores 0
		both("q15weight", k, 1.0/(q15v+1.0))
		both("ic14weight", k, 1.0/(float64(inter[k])+1.0))
		if n := inter[k]; n > 0 {
			both("interactsWith", k, 1.0/float64(n))
		}
		if w, ok := cohortW[k]; ok {
			both("cohort", k, w)
		}
	}
	return out, nil
}
