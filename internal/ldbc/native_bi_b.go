// Native BI kernels Q13, Q14, Q18-Q20 -- ports of rustychickpeas-ldbc
// src/bi/faithful_b.rs (zombies, international dialog, friend
// recommendation, and the two derived-weight shortest-path queries).

package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

func init() {
	registerNative("BI", "Q13", biQ13)
	registerNative("BI", "Q14", biQ14)
	registerNative("BI", "Q18", biQ18)
	registerNative("BI", "Q19", biQ19)
	registerNative("BI", "Q20", biQ20)
}

// biQ13 -- zombies in France (before 2013-01-01). Low-activity persons
// (under one message per month since account creation) scored by the
// share of their likes coming from other zombies; [personId,
// zombieLikeCount, totalLikeCount], likeRatio desc / id asc, top 100.
func biQ13(g *chickpeas.Snapshot) ([][]any, error) {
	country, ok := nodeByName(g, "Country", "France")
	if !ok {
		return [][]any{}, nil
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
	rows := make([][]any, 0, len(zombies))
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
		rows = append(rows, []any{i64At(idCol, z), zlc, tlc})
	}
	ratio := func(r []any) float64 {
		if r[2].(int64) == 0 {
			return 0
		}
		return float64(r[1].(int64)) / float64(r[2].(int64))
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpF64Desc(ratio(a), ratio(b)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ14 -- international dialog (Chile -> Argentina). Per city of
// country1, the best-scoring knows-pair (p1 in city, p2 in country2)
// where score rewards reply/like interaction kinds; [p1Id, p2Id,
// cityName, score], score desc / p1 asc / p2 asc, top 100.
func biQ14(g *chickpeas.Snapshot) ([][]any, error) {
	country1, ok1 := nodeByName(g, "Country", "Chile")
	country2, ok2 := nodeByName(g, "Country", "Argentina")
	if !ok1 || !ok2 {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	commentedOn := func(p chickpeas.NodeID) map[chickpeas.NodeID]bool {
		s := map[chickpeas.NodeID]bool{}
		for msg := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
			for parent := range g.Neighbors(msg, chickpeas.Outgoing, "REPLY_OF") {
				if cr, ok := creatorOf(g, parent); ok {
					s[cr] = true
				}
			}
		}
		return s
	}
	likedCreators := func(p chickpeas.NodeID) map[chickpeas.NodeID]bool {
		s := map[chickpeas.NodeID]bool{}
		for msg := range g.Neighbors(p, chickpeas.Outgoing, "LIKES") {
			if cr, ok := creatorOf(g, msg); ok {
				s[cr] = true
			}
		}
		return s
	}
	inC2 := map[chickpeas.NodeID]bool{}
	coC2 := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	lcC2 := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	for city := range g.Neighbors(country2, chickpeas.Incoming, "IS_PART_OF") {
		for p := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			if inC2[p] {
				continue
			}
			inC2[p] = true
			coC2[p] = commentedOn(p)
			lcC2[p] = likedCreators(p)
		}
	}
	var rows [][]any
	for city := range g.Neighbors(country1, chickpeas.Incoming, "IS_PART_OF") {
		cityName := strAt(g, city, "name")
		type cand struct {
			score, pa, pb int64
			found         bool
		}
		var best cand
		for p1 := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			p1co := commentedOn(p1)
			p1lc := likedCreators(p1)
			for p2 := range g.Neighbors(p1, chickpeas.Both, "KNOWS") {
				if !inC2[p2] {
					continue
				}
				var score int64
				if p1co[p2] {
					score += 4
				}
				if coC2[p2][p1] {
					score += 1
				}
				if p1lc[p2] {
					score += 10
				}
				if lcC2[p2][p1] {
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
			rows = append(rows, []any{best.pa, best.pb, cityName, best.score})
		}
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[3].(int64), b[3].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
			cmpI64Asc(a[1].(int64), b[1].(int64)),
		)
	}), nil
}

// biQ18 -- friend recommendation (Frank_Sinatra). Ordered pairs of
// tag-interested persons, not directly known, by distinct mutual
// friends; [p1Id, p2Id, mutualFriendCount], count desc / p1 asc / p2
// asc, top 20.
func biQ18(g *chickpeas.Snapshot) ([][]any, error) {
	tag, ok := nodeByName(g, "Tag", "Frank_Sinatra")
	if !ok {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	var interested []chickpeas.NodeID
	endpoints := nodeset.New()
	for p := range g.Neighbors(tag, chickpeas.Incoming, "HAS_INTEREST") {
		interested = append(interested, p)
		endpoints.Insert(p)
	}
	pairs := g.CommonNeighborCounts(interested, chickpeas.Both, g.Match("KNOWS"), endpoints)
	knowsOf := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	var rows [][]any
	for _, pr := range pairs {
		if pr.Source == pr.Target {
			continue
		}
		known := knowsOf[pr.Source]
		if known == nil {
			known = map[chickpeas.NodeID]bool{}
			for f := range g.Neighbors(pr.Source, chickpeas.Both, "KNOWS") {
				known[f] = true
			}
			knowsOf[pr.Source] = known
		}
		if known[pr.Target] {
			continue
		}
		rows = append(rows, []any{i64At(idCol, pr.Source), i64At(idCol, pr.Target), int64(pr.Count)})
	}
	return sortTruncate(rows, 20, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[2].(int64), b[2].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
			cmpI64Asc(a[1].(int64), b[1].(int64)),
		)
	}), nil
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
// asc, top 20.
func biQ19(g *chickpeas.Snapshot) ([][]any, error) {
	city1, ok1 := nodeByID(g, "City", 669)
	city2, ok2 := nodeByID(g, "City", 648)
	if !ok1 || !ok2 {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	interaction := buildInteractionMap(g)
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
	rows := chickpeas.ParNeighborFold(g, city1, chickpeas.Incoming, g.Match("IS_LOCATED_IN"),
		func() [][]any { return nil },
		func(acc [][]any, p1 chickpeas.NodeID) [][]any {
			for _, p2 := range c2 {
				if d, ok := g.WeightedShortestPath(p1, p2, chickpeas.Both, m, weight); ok && finite(d) {
					acc = append(acc, []any{i64At(idCol, p1), i64At(idCol, p2), d})
				}
			}
			return acc
		},
		func(a, b [][]any) [][]any { return append(a, b...) })
	return sortTruncate(rows, 20, func(a, b []any) bool {
		return cmpChain(
			cmpF64Asc(a[2].(float64), b[2].(float64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
			cmpI64Asc(a[1].(int64), b[1].(int64)),
		)
	}), nil
}

// studyRecord is one person's (university, classYear) enrolment.
type studyRecord struct {
	uni  chickpeas.NodeID
	year int64
}

// biQ20 -- recruitment (Falcon_Air -> person 66). Weighted shortest
// knows-path from each company employee to the target where edge weight
// is min |classYear difference|+1 over shared universities; [p1Id,
// pathWeight], weight asc / id asc, top 20.
func biQ20(g *chickpeas.Snapshot) ([][]any, error) {
	company, ok1 := nodeByName(g, "Company", "Falcon_Air")
	person2, ok2 := nodeByID(g, "Person", 66)
	if !ok1 || !ok2 {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	cyCol, ok := g.RelCol("classYear")
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
	weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
		if w, ok := wm[pairKey(from, rel.Neighbor)]; ok {
			return w
		}
		return inf
	}
	m := g.Match("KNOWS")
	var rows [][]any
	for p1 := range g.Neighbors(company, chickpeas.Incoming, "WORK_AT") {
		if p1 == person2 {
			continue
		}
		sp := g.DijkstraTo(p1, person2, chickpeas.Both, m, weight)
		if d, ok := sp.Distance(person2); ok && finite(d) {
			rows = append(rows, []any{i64At(idCol, p1), d})
		}
	}
	return sortTruncate(rows, 20, func(a, b []any) bool {
		return cmpChain(
			cmpF64Asc(a[1].(float64), b[1].(float64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}
