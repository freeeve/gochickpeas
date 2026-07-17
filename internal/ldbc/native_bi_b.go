// Native BI kernels Q13, Q14, Q18-Q20 -- ports of rustychickpeas-ldbc
// src/bi/faithful_b.rs (zombies, international dialog, friend
// recommendation, and the two derived-weight shortest-path queries).

package ldbc

import (
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
	"github.com/freeeve/gochickpeas/nodeset"
)

func init() {
	registerNativeV("BI", "Q13", simpleKernelV(biQ13))
	registerNativeV("BI", "Q14", simpleKernelV(biQ14))
	registerNativeV("BI", "Q18", simpleKernelV(biQ18))
	registerNativeV("BI", "Q19", biQ19)
	registerNativeV("BI", "Q20", biQ20)
}

// biQ13 -- zombies in France (before 2013-01-01). Low-activity persons
// (under one message per month since account creation) scored by the
// share of their likes coming from other zombies; [personId,
// zombieLikeCount, totalLikeCount], likeRatio desc / id asc, top 100.
func biQ13(g *chickpeas.Snapshot) ([][]value.Value, error) {
	country, ok := nodeByName(g, "Country", "France")
	if !ok {
		return [][]value.Value{}, nil
	}
	endDay := dayFromCivil(2013, 1, 1)
	endYM := int64(2013*12 + 1)
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	pdayCol, err := nodeI64Col(g, "pday")
	if err != nil {
		return nil, err
	}
	pymCol, err := nodeI64Col(g, "pym")
	if err != nil {
		return nil, err
	}
	persons, _ := g.NodesWithLabel("Person")
	zombies := map[chickpeas.NodeID]bool{}
	for city := range g.Neighbors(country, chickpeas.Incoming, "IS_PART_OF") {
		for p := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			if persons == nil || !persons.Contains(p) {
				continue
			}
			if i64At(pdayCol, p) >= endDay {
				continue
			}
			var mcount int64
			for m := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
				if i64At(dayCol, m) < endDay {
					mcount++
				}
			}
			months := endYM - i64At(pymCol, p) + 1
			if months > 0 && mcount < months {
				zombies[p] = true
			}
		}
	}
	rows := make([][]value.Value, 0, len(zombies))
	for z := range zombies {
		var zlc, tlc int64
		for m := range g.Neighbors(z, chickpeas.Incoming, "HAS_CREATOR") {
			for liker := range g.Neighbors(m, chickpeas.Incoming, "LIKES") {
				if i64At(pdayCol, liker) < endDay {
					tlc++
				}
				if zombies[liker] {
					zlc++
				}
			}
		}
		rows = append(rows, []value.Value{value.Int(i64At(idCol, z)), value.Int(zlc), value.Int(tlc)})
	}
	ratio := func(r []value.Value) float64 {
		zlc, _ := r[1].AsInt()
		tlc, _ := r[2].AsInt()
		if tlc == 0 {
			return 0
		}
		return float64(zlc) / float64(tlc)
	}
	return sortTruncate(rows, 100, func(a, b []value.Value) bool {
		a0, _ := a[0].AsInt()
		b0, _ := b[0].AsInt()
		return cmpChain(
			cmpF64Desc(ratio(a), ratio(b)),
			cmpI64Asc(a0, b0),
		)
	}), nil
}

// biQ14 -- international dialog (Chile -> Argentina). Per city of
// country1, the best-scoring knows-pair (p1 in city, p2 in country2)
// where score rewards reply/like interaction kinds; [p1Id, p2Id,
// cityName, score], score desc / p1 asc / p2 asc, top 100.
func biQ14(g *chickpeas.Snapshot) ([][]value.Value, error) {
	country1, ok1 := nodeByName(g, "Country", "Chile")
	country2, ok2 := nodeByName(g, "Country", "Argentina")
	if !ok1 || !ok2 {
		return [][]value.Value{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	// Interaction membership as two flat pair-keyed probe sets shared by
	// BOTH endpoints' directions -- commented(a->b) probes co.Has(a<<32|b)
	// -- replacing a fresh map per person on each side of the loop.
	var co, lc flatset.U64Set
	var indexed flatset.U32Set
	indexPerson := func(p chickpeas.NodeID) {
		if !indexed.Add(uint32(p)) {
			return
		}
		for msg := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
			for parent := range g.Neighbors(msg, chickpeas.Outgoing, "REPLY_OF") {
				if cr, ok := creatorOf(g, parent); ok {
					co.Add(uint64(uint32(p))<<32 | uint64(uint32(cr)))
				}
			}
		}
		for msg := range g.Neighbors(p, chickpeas.Outgoing, "LIKES") {
			if cr, ok := creatorOf(g, msg); ok {
				lc.Add(uint64(uint32(p))<<32 | uint64(uint32(cr)))
			}
		}
	}
	var inC2 flatset.U32Set
	for city := range g.Neighbors(country2, chickpeas.Incoming, "IS_PART_OF") {
		for p := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			if inC2.Add(uint32(p)) {
				indexPerson(p)
			}
		}
	}
	var rows [][]value.Value
	for city := range g.Neighbors(country1, chickpeas.Incoming, "IS_PART_OF") {
		cityName := strAt(g, city, "name")
		type cand struct {
			score, pa, pb int64
			found         bool
		}
		var best cand
		for p1 := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			indexPerson(p1)
			for p2 := range g.Neighbors(p1, chickpeas.Both, "KNOWS") {
				if !inC2.Has(uint32(p2)) {
					continue
				}
				var score int64
				if co.Has(uint64(uint32(p1))<<32 | uint64(uint32(p2))) {
					score += 4
				}
				if co.Has(uint64(uint32(p2))<<32 | uint64(uint32(p1))) {
					score += 1
				}
				if lc.Has(uint64(uint32(p1))<<32 | uint64(uint32(p2))) {
					score += 10
				}
				if lc.Has(uint64(uint32(p2))<<32 | uint64(uint32(p1))) {
					score += 1
				}
				pa, pb := i64At(idCol, p1), i64At(idCol, p2)
				// Keep the incumbent unless the candidate wins
				// (score desc, p1 id asc, p2 id asc).
				if best.found && !cmpChain(
					cmpI64Desc(score, best.score),
					cmpI64Asc(pa, best.pa),
					cmpI64Asc(pb, best.pb),
				) {
					continue
				}
				best = cand{score, pa, pb, true}
			}
		}
		if best.found {
			rows = append(rows, []value.Value{
				value.Int(best.pa), value.Int(best.pb), value.Str(cityName), value.Int(best.score),
			})
		}
	}
	return sortTruncate(rows, 100, func(a, b []value.Value) bool {
		a3, _ := a[3].AsInt()
		b3, _ := b[3].AsInt()
		a0, _ := a[0].AsInt()
		b0, _ := b[0].AsInt()
		a1, _ := a[1].AsInt()
		b1, _ := b[1].AsInt()
		return cmpChain(
			cmpI64Desc(a3, b3),
			cmpI64Asc(a0, b0),
			cmpI64Asc(a1, b1),
		)
	}), nil
}

// biQ18 -- friend recommendation (Frank_Sinatra). Ordered pairs of
// tag-interested persons, not directly known, by distinct mutual
// friends; [p1Id, p2Id, mutualFriendCount], count desc / p1 asc / p2
// asc, top 20.
func biQ18(g *chickpeas.Snapshot) ([][]value.Value, error) {
	tag, ok := nodeByName(g, "Tag", "Frank_Sinatra")
	if !ok {
		return [][]value.Value{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	// Bulk-build the endpoint set from the sorted id list: per-person
	// Insert paid roaring container work on every add.
	var interested []chickpeas.NodeID
	var epIDs []uint32
	for p := range g.Neighbors(tag, chickpeas.Incoming, "HAS_INTEREST") {
		interested = append(interested, p)
		epIDs = append(epIDs, uint32(p))
	}
	slices.Sort(epIDs)
	endpoints := nodeset.Of(epIDs...)
	pairs := g.CommonNeighborCounts(interested, chickpeas.Both, g.Match("KNOWS"), endpoints)
	// Collect survivors as typed rows, sort+truncate typed, and box only the
	// top 20 -- the candidate set is far larger than the output, so boxing
	// every pair into []any before truncation dominated the allocations.
	type cand struct {
		id1, id2, count int64
	}
	var cands []cand
	// One reused KNOWS-membership set, rebuilt when the source changes
	// (CommonNeighborCounts groups a source's pairs contiguously; a reordering
	// would only rebuild redundantly, never mis-filter). clear() reuses the
	// map's buckets, so no per-source map allocation.
	known := map[chickpeas.NodeID]bool{}
	var lastSource chickpeas.NodeID
	haveSource := false
	for _, pr := range pairs {
		if pr.Source == pr.Target {
			continue
		}
		if !haveSource || pr.Source != lastSource {
			clear(known)
			for f := range g.Neighbors(pr.Source, chickpeas.Both, "KNOWS") {
				known[f] = true
			}
			lastSource, haveSource = pr.Source, true
		}
		if known[pr.Target] {
			continue
		}
		cands = append(cands, cand{i64At(idCol, pr.Source), i64At(idCol, pr.Target), int64(pr.Count)})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(
			cmpI64Desc(a.count, b.count),
			cmpI64Asc(a.id1, b.id1),
			cmpI64Asc(a.id2, b.id2),
		)
	})
	if len(cands) > 20 {
		cands = cands[:20]
	}
	cells := make([]value.Value, len(cands)*3)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*3] = value.Int(c.id1)
		cells[i*3+1] = value.Int(c.id2)
		cells[i*3+2] = value.Int(c.count)
		rows[i] = cells[i*3 : i*3+3 : i*3+3]
	}
	return rows, nil
}

// interactionKey is the undirected person-pair key of Q19's projected
// interaction graph.
type interactionKey struct{ lo, hi chickpeas.NodeID }

func pairKey(a, b chickpeas.NodeID) interactionKey {
	if a < b {
		return interactionKey{a, b}
	}
	return interactionKey{b, a}
}

// buildInteractionMap counts reply interactions between distinct
// message creators per undirected pair (Q19's weight input).
func buildInteractionMap(g *chickpeas.Snapshot) map[interactionKey]uint32 {
	interaction := map[interactionKey]uint32{}
	comments, ok := g.NodesWithLabel("Comment")
	if !ok {
		return interaction
	}
	for c := range comments.Iter() {
		a, ok := creatorOf(g, c)
		if !ok {
			continue
		}
		for parent := range g.Neighbors(c, chickpeas.Outgoing, "REPLY_OF") {
			if b, ok := creatorOf(g, parent); ok && a != b {
				interaction[pairKey(a, b)]++
			}
		}
	}
	return interaction
}

// biQ19 -- interaction path between cities (669 <-> 648). Weighted
// shortest knows-path per (p1 in city1, p2 in city2) with weight
// 1/interactions; [p1Id, p2Id, totalWeight], weight asc / p1 asc / p2
// asc, top 20. The interaction map is built in the untimed prepare
// phase, matching the rcp-native harness (its timer sees only the
// per-pair searches).
func biQ19(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	interaction := buildInteractionMap(g)
	return func() ([][]value.Value, error) {
		city1, ok1 := nodeByID(g, "City", 669)
		city2, ok2 := nodeByID(g, "City", 648)
		if !ok1 || !ok2 {
			return [][]value.Value{}, nil
		}
		weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
			if n := interaction[pairKey(from, rel.Neighbor)]; n > 0 {
				return 1.0 / float64(n)
			}
			return inf
		}
		var c2 []chickpeas.NodeID
		for p := range g.Neighbors(city2, chickpeas.Incoming, "IS_LOCATED_IN") {
			c2 = append(c2, p)
		}
		m := g.Match("KNOWS")
		// Typed candidate rows, boxed into flat cells only after the top-20
		// truncation: a slice per surviving pair dominated the kernel.
		type q19Row struct {
			p1, p2 int64
			d      float64
		}
		cands := chickpeas.ParNeighborFold(g, city1, chickpeas.Incoming, g.Match("IS_LOCATED_IN"),
			func() []q19Row { return nil },
			func(acc []q19Row, p1 chickpeas.NodeID) []q19Row {
				for _, p2 := range c2 {
					if d, ok := g.WeightedShortestPath(p1, p2, chickpeas.Both, m, weight); ok && finite(d) {
						acc = append(acc, q19Row{i64At(idCol, p1), i64At(idCol, p2), d})
					}
				}
				return acc
			},
			func(a, b []q19Row) []q19Row { return append(a, b...) })
		sortByLess(cands, func(a, b q19Row) bool {
			return cmpChain(
				cmpF64Asc(a.d, b.d),
				cmpI64Asc(a.p1, b.p1),
				cmpI64Asc(a.p2, b.p2),
			)
		})
		if len(cands) > 20 {
			cands = cands[:20]
		}
		cells := make([]value.Value, len(cands)*3)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*3] = value.Int(c.p1)
			cells[i*3+1] = value.Int(c.p2)
			cells[i*3+2] = value.Float(c.d)
			rows[i] = cells[i*3 : i*3+3 : i*3+3]
		}
		return rows, nil
	}, nil
}

// studyRecord is one person's (university, classYear) enrolment.
type studyRecord struct {
	uni  chickpeas.NodeID
	year int64
}

// biQ20 -- recruitment (Falcon_Air -> person 66). Weighted shortest
// knows-path from each company employee to the target where edge weight
// is min |classYear difference|+1 over shared universities; [p1Id,
// pathWeight], weight asc / id asc, top 20. The study records and the
// cohort weight map are built in the untimed prepare phase, matching
// the rcp-native harness.
func biQ20(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	wm, err := cohortWeightMap(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		company, ok1 := nodeByName(g, "Company", "Falcon_Air")
		person2, ok2 := nodeByID(g, "Person", 66)
		if !ok1 || !ok2 {
			return [][]value.Value{}, nil
		}
		weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
			if w, ok := wm[pairKey(from, rel.Neighbor)]; ok {
				return w
			}
			return inf
		}
		m := g.Match("KNOWS")
		var rows [][]value.Value
		for p1 := range g.Neighbors(company, chickpeas.Incoming, "WORK_AT") {
			if p1 == person2 {
				continue
			}
			// Point-to-point per employee: the bidirectional
			// WeightedShortestPath (symmetric cohort weight via the
			// undirected pairKey) replaces the one-directional Dijkstra.
			if d, ok := g.WeightedShortestPath(p1, person2, chickpeas.Both, m, weight); ok && finite(d) {
				rows = append(rows, []value.Value{value.Int(i64At(idCol, p1)), value.Float(d)})
			}
		}
		return sortTruncate(rows, 20, func(a, b []value.Value) bool {
			a1, _ := a[1].AsFloat()
			b1, _ := b[1].AsFloat()
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			return cmpChain(
				cmpF64Asc(a1, b1),
				cmpI64Asc(a0, b0),
			)
		}), nil
	}, nil
}
