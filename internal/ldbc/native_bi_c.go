// Native BI kernels Q3, Q4, Q10, Q15-Q17 -- ports of
// rustychickpeas-ldbc src/bi/faithful_c.rs (forum reply-tree walks,
// social-circle experts, the forum-window weighted path, fake-news
// detection, and information propagation).

package ldbc

import (
	chickpeas "github.com/freeeve/gochickpeas"
)

func init() {
	registerNative("BI", "Q3", simpleKernel(biQ3))
	registerNative("BI", "Q4", simpleKernel(biQ4))
	registerNative("BI", "Q10", simpleKernel(biQ10))
	registerNative("BI", "Q15", simpleKernel(biQ15))
	registerNative("BI", "Q16", simpleKernel(biQ16))
	registerNative("BI", "Q17", simpleKernel(biQ17))
}

// biQ3 -- popular topics in a country (Burma, MusicalArtist). Forums
// moderated from the country counted by distinct reply-tree messages
// carrying a class tag; [forumId, title, forumCreationDate(ms), modId,
// messageCount], count desc / forumId asc, top 20. The creationDate
// column is epoch-ms against an epoch-day ref (norm col2:msday).
func biQ3(g *chickpeas.Snapshot) ([][]any, error) {
	country, ok1 := nodeByName(g, "Country", "Burma")
	tc, ok2 := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok1 || !ok2 {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	cdCol, err := nodeI64Col(g, "creationDate")
	if err != nil {
		return nil, err
	}
	classTags := map[chickpeas.NodeID]bool{}
	for t := range g.Neighbors(tc, chickpeas.Incoming, "HAS_TYPE") {
		classTags[t] = true
	}
	hasClassTag := func(msg chickpeas.NodeID) bool {
		for t := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_TAG") {
			if classTags[t] {
				return true
			}
		}
		return false
	}
	type cand struct {
		forumID, cdate, personID, count int64
		title                           string
	}
	var cands []cand
	// Reused across the forum/post walk: msgs (distinct class-tagged messages
	// per forum, cleared per forum) and stack (reply-tree DFS, reset per post).
	msgs := map[chickpeas.NodeID]bool{}
	var stack []chickpeas.NodeID
	for city := range g.Neighbors(country, chickpeas.Incoming, "IS_PART_OF") {
		for person := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			for forum := range g.Neighbors(person, chickpeas.Incoming, "HAS_MODERATOR") {
				clear(msgs)
				for post := range g.Neighbors(forum, chickpeas.Outgoing, "CONTAINER_OF") {
					stack = append(stack[:0], post)
					for len(stack) > 0 {
						n := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						if hasClassTag(n) {
							msgs[n] = true
						}
						for c := range g.Neighbors(n, chickpeas.Incoming, "REPLY_OF") {
							stack = append(stack, c)
						}
					}
				}
				if len(msgs) > 0 {
					cands = append(cands, cand{
						i64At(idCol, forum), i64At(cdCol, forum),
						i64At(idCol, person), int64(len(msgs)), strAt(g, forum, "title"),
					})
				}
			}
		}
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.count, b.count), cmpI64Asc(a.forumID, b.forumID))
	})
	if len(cands) > 20 {
		cands = cands[:20]
	}
	rows := make([][]any, len(cands))
	for i, c := range cands {
		rows[i] = []any{c.forumID, c.title, c.cdate, c.personID, c.count}
	}
	return rows, nil
}

// biQ4 -- top message creators (forums created after 2010-01-29). The
// top-100 forums by largest single-country member cohort, then all
// their members ranked by messages created in those forums' reply
// trees; [personId, messageCount], count desc / id asc, top 100.
func biQ4(g *chickpeas.Snapshot) ([][]any, error) {
	afterDay := dayFromCivil(2010, 1, 29)
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	fdayCol, err := nodeI64Col(g, "fday")
	if err != nil {
		return nil, err
	}
	var forums []chickpeas.NodeID
	if fs, ok := g.NodesWithLabel("Forum"); ok {
		for f := range fs.Iter() {
			if i64At(fdayCol, f) > afterDay {
				forums = append(forums, f)
			}
		}
	}
	top := g.NeighborGroups(forums, g.Match("HAS_MEMBER"), chickpeas.Outgoing).
		Project(
			chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_LOCATED_IN"},
			chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_PART_OF"},
		).
		TopBySize(100, "id")
	members := map[chickpeas.NodeID]bool{}
	for _, s := range top {
		for m := range g.Neighbors(s.Source, chickpeas.Outgoing, "HAS_MEMBER") {
			members[m] = true
		}
	}
	msgCount := map[chickpeas.NodeID]int64{}
	var stack []chickpeas.NodeID
	for _, s := range top {
		for post := range g.Neighbors(s.Source, chickpeas.Outgoing, "CONTAINER_OF") {
			stack = append(stack, post)
		}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if creator, ok := creatorOf(g, n); ok && members[creator] {
				msgCount[creator]++
			}
			for c := range g.Neighbors(n, chickpeas.Incoming, "REPLY_OF") {
				stack = append(stack, c)
			}
		}
	}
	// Typed top-k: members spans every member of the top-100 forums, far more
	// than the 100 output rows, so box only the survivors.
	type cand struct{ id, count int64 }
	cands := make([]cand, 0, len(members))
	for p := range members {
		cands = append(cands, cand{i64At(idCol, p), msgCount[p]})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.count, b.count), cmpI64Asc(a.id, b.id))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	rows := make([][]any, len(cands))
	for i, c := range cands {
		rows[i] = []any{c.id, c.count}
	}
	return rows, nil
}

// biQ10 -- experts in social circle (person 3470, China, MusicalArtist,
// knows-distance 3..4). Distinct tagged messages per (expert, tag) where
// the message carries any class tag; [personId, tagName, messageCount],
// count desc / name asc / id asc, top 100.
func biQ10(g *chickpeas.Snapshot) ([][]any, error) {
	start, ok := nodeByID(g, "Person", 3470)
	if !ok {
		return [][]any{}, nil
	}
	const minDist, maxDist = 3, 4
	country, ok1 := nodeByName(g, "Country", "China")
	tc, ok2 := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok1 || !ok2 {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	dist := g.BFSDistances(start, chickpeas.Both, g.Match("KNOWS"), maxDist)
	inCountry := personsOfCountry(g, country)
	classTags := map[chickpeas.NodeID]bool{}
	for t := range g.Neighbors(tc, chickpeas.Incoming, "HAS_TYPE") {
		classTags[t] = true
	}
	// Distinct messages per (expert, tag), counted via a flat (expert, tag,
	// message) dedup set plus a per-group counter incremented on first sight,
	// rather than a map-of-maps: the inner sets were only read for their
	// length, so this avoids allocating an inner map per (expert, tag) group.
	type expertTag struct{ expert, tag chickpeas.NodeID }
	type expertTagMsg struct{ expert, tag, msg chickpeas.NodeID }
	seen := map[expertTagMsg]bool{}
	counts := map[expertTag]int64{}
	var tags []chickpeas.NodeID // message's tag list, reused per message
	for expert, d := range dist {
		if d < minDist || d > maxDist || !inCountry[expert] {
			continue
		}
		for msg := range g.Neighbors(expert, chickpeas.Incoming, "HAS_CREATOR") {
			tags = tags[:0]
			anyClass := false
			for t := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_TAG") {
				if classTags[t] {
					anyClass = true
				}
				tags = append(tags, t)
			}
			if !anyClass {
				continue
			}
			for _, t := range tags {
				triple := expertTagMsg{expert, t, msg}
				if !seen[triple] {
					seen[triple] = true
					counts[expertTag{expert, t}]++
				}
			}
		}
	}
	// Typed top-k: box only the top 100 of every (expert,tag) group.
	type cand struct {
		id, count int64
		name      string
	}
	cands := make([]cand, 0, len(counts))
	for k, c := range counts {
		cands = append(cands, cand{i64At(idCol, k.expert), c, strAt(g, k.tag, "name")})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.count, b.count), cmpStrAsc(a.name, b.name), cmpI64Asc(a.id, b.id))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	rows := make([][]any, len(cands))
	for i, c := range cands {
		rows[i] = []any{c.id, c.name, c.count}
	}
	return rows, nil
}

// biQ15 -- weighted interaction path (persons 14 -> 16, forums created
// 2010-11-01..2010-12-01). Reply interactions whose thread forum falls
// in the window weight the knows graph (post reply 1.0, comment reply
// 0.5); edge weight 1/(w+1); [[cost]] or [[-1.0]] when unreachable.
func biQ15(g *chickpeas.Snapshot) ([][]any, error) {
	src, ok1 := nodeByID(g, "Person", 14)
	tgt, ok2 := nodeByID(g, "Person", 16)
	if !ok1 || !ok2 {
		return [][]any{{-1.0}}, nil
	}
	w, err := q15WeightMap(g)
	if err != nil {
		return nil, err
	}
	weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
		return 1.0 / (w[pairKey(from, rel.Neighbor)] + 1.0)
	}
	sp := g.DijkstraTo(src, tgt, chickpeas.Both, g.Match("KNOWS"), weight)
	if d, ok := sp.Distance(tgt); ok && finite(d) {
		return [][]any{{d}}, nil
	}
	return [][]any{{-1.0}}, nil
}

// biQ16Param -- persons who made a message with the tag on the day and
// have at most maxKnows friends who did the same, with message counts.
func biQ16Param(g *chickpeas.Snapshot, tagName string, day, maxKnows int64) (map[chickpeas.NodeID]int64, error) {
	tag, ok := nodeByName(g, "Tag", tagName)
	if !ok {
		return map[chickpeas.NodeID]int64{}, nil
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	cm := map[chickpeas.NodeID]int64{}
	creatorsOnDay := map[chickpeas.NodeID]bool{}
	for msg := range g.Neighbors(tag, chickpeas.Incoming, "HAS_TAG") {
		if i64At(dayCol, msg) != day {
			continue
		}
		for creator := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_CREATOR") {
			cm[creator]++
			creatorsOnDay[creator] = true
		}
	}
	out := map[chickpeas.NodeID]int64{}
	for p1, c := range cm {
		var friends int64
		for f := range g.Neighbors(p1, chickpeas.Both, "KNOWS") {
			if creatorsOnDay[f] {
				friends++
			}
		}
		if friends <= maxKnows {
			out[p1] = c
		}
	}
	return out, nil
}

// biQ16 -- fake news detection (Meryl_Streep@2012-09-16 AND
// Hank_Williams@2012-05-08, maxKnows 4). Persons qualifying for both
// params; [personId, countA, countB], (a+b) desc / id asc, top 20.
// The official params select zero rows at SF1 (the ref is empty).
func biQ16(g *chickpeas.Snapshot) ([][]any, error) {
	ra, err := biQ16Param(g, "Meryl_Streep", dayFromCivil(2012, 9, 16), 4)
	if err != nil {
		return nil, err
	}
	rb, err := biQ16Param(g, "Hank_Williams", dayFromCivil(2012, 5, 8), 4)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	rows := [][]any{}
	for p, ca := range ra {
		if cb, ok := rb[p]; ok {
			rows = append(rows, []any{i64At(idCol, p), ca, cb})
		}
	}
	return sortTruncate(rows, 20, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64)+a[2].(int64), b[1].(int64)+b[2].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ17 -- information propagation (Slavoj_Žižek, delta 4h). Distinct
// message2 per person1 where person1's tagged message1 sits in forum1;
// a forum1 member p2 posted a tagged comment replying to tagged
// message2 (by another forum1 member p3) in a different forum2, more
// than delta after message1, with person1 not a member of forum2;
// [personId, messageCount], count desc / id asc, top 10.
func biQ17(g *chickpeas.Snapshot) ([][]any, error) {
	tag, ok := nodeByName(g, "Tag", "Slavoj_Žižek")
	if !ok {
		return [][]any{}, nil
	}
	const deltaMS = 4 * 3_600_000
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	var roots chickpeas.RootsVia
	if rt, ok := g.RelType("REPLY_OF"); ok {
		roots = g.RootsVia(rt, chickpeas.Outgoing)
	}
	forumOf := func(m chickpeas.NodeID) (chickpeas.NodeID, bool) {
		root := m
		if roots != nil {
			root = roots[m]
		}
		return g.FirstNeighbor(root, chickpeas.Incoming, "CONTAINER_OF")
	}
	var tagged []chickpeas.NodeID
	taggedSet := map[chickpeas.NodeID]bool{}
	for m := range g.Neighbors(tag, chickpeas.Incoming, "HAS_TAG") {
		tagged = append(tagged, m)
		taggedSet[m] = true
	}
	type m1Rec struct {
		p1  chickpeas.NodeID
		ms1 int64
	}
	type candRec struct {
		p2, p3, msg2, f2 chickpeas.NodeID
		ms2              int64
	}
	m1ByForum := map[chickpeas.NodeID][]m1Rec{}
	var cands []candRec
	relevant := map[chickpeas.NodeID]bool{}
	for _, m := range tagged {
		if p1, ok := creatorOf(g, m); ok {
			if f1, ok := forumOf(m); ok {
				m1ByForum[f1] = append(m1ByForum[f1], m1Rec{p1, i64At(msCol, m)})
				relevant[f1] = true
			}
		}
		msg2, ok := g.FirstNeighbor(m, chickpeas.Outgoing, "REPLY_OF")
		if !ok || !taggedSet[msg2] {
			continue
		}
		p2, ok1 := creatorOf(g, m)
		p3, ok2 := creatorOf(g, msg2)
		f2, ok3 := forumOf(msg2)
		if ok1 && ok2 && ok3 {
			cands = append(cands, candRec{p2, p3, msg2, f2, i64At(msCol, msg2)})
			relevant[f2] = true
		}
	}
	pm := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	for f := range relevant {
		for p := range g.Neighbors(f, chickpeas.Outgoing, "HAS_MEMBER") {
			set := pm[p]
			if set == nil {
				set = map[chickpeas.NodeID]bool{}
				pm[p] = set
			}
			set[f] = true
		}
	}
	// Distinct msg2 per person, counted via a flat (person, msg2) pair-set plus
	// a per-person counter incremented on first sight, rather than an inner map
	// per person: these inner sets were only read for their length. (pm above
	// stays a map-of-maps -- it is probed for membership in the hot loop.)
	type pMsg struct{ p1, msg chickpeas.NodeID }
	seen := map[pMsg]bool{}
	counts := map[chickpeas.NodeID]int64{}
	for _, c := range cands {
		if c.p2 == c.p3 {
			continue
		}
		fp2, fp3 := pm[c.p2], pm[c.p3]
		for f1 := range fp2 {
			if f1 == c.f2 || !fp3[f1] {
				continue
			}
			for _, m1 := range m1ByForum[f1] {
				if c.ms2 > m1.ms1+deltaMS && !pm[m1.p1][c.f2] {
					pair := pMsg{m1.p1, c.msg2}
					if !seen[pair] {
						seen[pair] = true
						counts[m1.p1]++
					}
				}
			}
		}
	}
	type outRow struct{ id, count int64 }
	ranked := make([]outRow, 0, len(counts))
	for p, cnt := range counts {
		ranked = append(ranked, outRow{i64At(idCol, p), cnt})
	}
	sortByLess(ranked, func(a, b outRow) bool {
		return cmpChain(cmpI64Desc(a.count, b.count), cmpI64Asc(a.id, b.id))
	})
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}
	rows := make([][]any, len(ranked))
	for i, c := range ranked {
		rows[i] = []any{c.id, c.count}
	}
	return rows, nil
}
