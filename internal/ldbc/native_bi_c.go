// Native BI kernels Q3, Q4, Q10, Q15-Q17 -- ports of
// rustychickpeas-ldbc src/bi/faithful_c.rs (forum reply-tree walks,
// social-circle experts, the forum-window weighted path, fake-news
// detection, and information propagation).

package ldbc

import (
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
)

func init() {
	registerNativeV("BI", "Q3", simpleKernelV(biQ3))
	registerNativeV("BI", "Q4", simpleKernelV(biQ4))
	registerNativeV("BI", "Q10", simpleKernelV(biQ10))
	registerNativeV("BI", "Q15", simpleKernelV(biQ15))
	registerNativeV("BI", "Q16", simpleKernelV(biQ16))
	registerNativeV("BI", "Q17", simpleKernelV(biQ17))
}

// biQ3 -- popular topics in a country (Burma, MusicalArtist). Forums
// moderated from the country counted by distinct reply-tree messages
// carrying a class tag; [forumId, title, forumCreationDate(ms), modId,
// messageCount], count desc / forumId asc, top 20. The creationDate
// column is epoch-ms against an epoch-day ref (norm col2:msday).
func biQ3(g *chickpeas.Snapshot) ([][]value.Value, error) {
	country, ok1 := nodeByName(g, "Country", "Burma")
	tc, ok2 := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok1 || !ok2 {
		return [][]value.Value{}, nil
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
	cells := make([]value.Value, len(cands)*5)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*5] = value.Int(c.forumID)
		cells[i*5+1] = value.Str(c.title)
		cells[i*5+2] = value.Int(c.cdate)
		cells[i*5+3] = value.Int(c.personID)
		cells[i*5+4] = value.Int(c.count)
		rows[i] = cells[i*5 : i*5+5 : i*5+5]
	}
	return rows, nil
}

// biQ4 -- top message creators (forums created after 2010-01-29). The
// top-100 forums by largest single-country member cohort, then all
// their members ranked by messages created in those forums' reply
// trees; [personId, messageCount], count desc / id asc, top 100.
func biQ4(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
	cells := make([]value.Value, len(cands)*2)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*2] = value.Int(c.id)
		cells[i*2+1] = value.Int(c.count)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}

// biQ10 -- experts in social circle (person 3470, China, MusicalArtist,
// knows-distance 3..4). Distinct tagged messages per (expert, tag) where
// the message carries any class tag; [personId, tagName, messageCount],
// count desc / name asc / id asc, top 100.
func biQ10(g *chickpeas.Snapshot) ([][]value.Value, error) {
	start, ok := nodeByID(g, "Person", 3470)
	if !ok {
		return [][]value.Value{}, nil
	}
	const minDist, maxDist = 3, 4
	country, ok1 := nodeByName(g, "Country", "China")
	tc, ok2 := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok1 || !ok2 {
		return [][]value.Value{}, nil
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
	cells := make([]value.Value, len(cands)*3)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*3] = value.Int(c.id)
		cells[i*3+1] = value.Str(c.name)
		cells[i*3+2] = value.Int(c.count)
		rows[i] = cells[i*3 : i*3+3 : i*3+3]
	}
	return rows, nil
}

// biQ15 -- weighted interaction path (persons 14 -> 16, forums created
// 2010-11-01..2010-12-01). Reply interactions whose thread forum falls
// in the window weight the knows graph (post reply 1.0, comment reply
// 0.5); edge weight 1/(w+1); [[cost]] or [[-1.0]] when unreachable.
// A single endpoint pair, so the point-to-point WeightedShortestPath
// (bidirectional meet-in-the-middle -- the native twin of the gql plan's
// WeightedShortestPath operator) replaces the one-directional Dijkstra;
// the undirected pairKey makes the weight symmetric, as that search
// requires.
func biQ15(g *chickpeas.Snapshot) ([][]value.Value, error) {
	src, ok1 := nodeByID(g, "Person", 14)
	tgt, ok2 := nodeByID(g, "Person", 16)
	if !ok1 || !ok2 {
		return [][]value.Value{{value.Float(-1.0)}}, nil
	}
	w, err := q15WeightMap(g)
	if err != nil {
		return nil, err
	}
	weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
		return 1.0 / (w[pairKey(from, rel.Neighbor)] + 1.0)
	}
	if d, ok := g.WeightedShortestPath(src, tgt, chickpeas.Both, g.Match("KNOWS"), weight); ok && finite(d) {
		return [][]value.Value{{value.Float(d)}}, nil
	}
	return [][]value.Value{{value.Float(-1.0)}}, nil
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
func biQ16(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
	rows := [][]value.Value{}
	for p, ca := range ra {
		if cb, ok := rb[p]; ok {
			rows = append(rows, []value.Value{value.Int(i64At(idCol, p)), value.Int(ca), value.Int(cb)})
		}
	}
	return sortTruncate(rows, 20, func(a, b []value.Value) bool {
		a1, _ := a[1].AsInt()
		a2, _ := a[2].AsInt()
		b1, _ := b[1].AsInt()
		b2, _ := b[2].AsInt()
		a0, _ := a[0].AsInt()
		b0, _ := b[0].AsInt()
		return cmpChain(
			cmpI64Desc(a1+a2, b1+b2),
			cmpI64Asc(a0, b0),
		)
	}), nil
}

// biQ17 -- information propagation (Slavoj_Žižek, delta 4h). Distinct
// message2 per person1 where person1's tagged message1 sits in forum1;
// a forum1 member p2 posted a tagged comment replying to tagged
// message2 (by another forum1 member p3) in a different forum2, more
// than delta after message1, with person1 not a member of forum2;
// [personId, messageCount], count desc / id asc, top 10.
func biQ17(g *chickpeas.Snapshot) ([][]value.Value, error) {
	tag, ok := nodeByName(g, "Tag", "Slavoj_Žižek")
	if !ok {
		return [][]value.Value{}, nil
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
		f1  chickpeas.NodeID
		p1  chickpeas.NodeID
		ms1 int64
	}
	type candRec struct {
		p2, p3, msg2, f2 chickpeas.NodeID
		ms2              int64
	}
	var m1s []m1Rec
	var cands []candRec
	var relevantSet flatset.U32Set
	var relevant []chickpeas.NodeID
	for _, m := range tagged {
		if p1, ok := creatorOf(g, m); ok {
			if f1, ok := forumOf(m); ok {
				m1s = append(m1s, m1Rec{f1, p1, i64At(msCol, m)})
				if relevantSet.Add(uint32(f1)) {
					relevant = append(relevant, f1)
				}
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
			if relevantSet.Add(uint32(f2)) {
				relevant = append(relevant, f2)
			}
		}
	}
	// m1s sorted by forum: the per-forum record lookup is a span of one
	// flat slice (a map of per-forum slices costs an allocation per forum
	// plus growth per append).
	sortByLess(m1s, func(a, b m1Rec) bool { return a.f1 < b.f1 })
	m1Span := func(f chickpeas.NodeID) []m1Rec {
		lo := sort.Search(len(m1s), func(i int) bool { return m1s[i].f1 >= f })
		hi := sort.Search(len(m1s), func(i int) bool { return m1s[i].f1 > f })
		return m1s[lo:hi]
	}
	// Person-forum membership as one sorted (person<<32|forum) key slice
	// (span iteration) plus a flat probe set (O(1) membership) -- the
	// map-of-maps form allocated an inner map per member, half this
	// kernel's allocations.
	var pfPairs []uint64
	var pfSet flatset.U64Set
	for _, f := range relevant {
		for p := range g.Neighbors(f, chickpeas.Outgoing, "HAS_MEMBER") {
			key := uint64(uint32(p))<<32 | uint64(uint32(f))
			if pfSet.Add(key) {
				pfPairs = append(pfPairs, key)
			}
		}
	}
	slices.Sort(pfPairs)
	forumsOf := func(p chickpeas.NodeID) []uint64 {
		lo := sort.Search(len(pfPairs), func(i int) bool { return pfPairs[i] >= uint64(uint32(p))<<32 })
		hi := sort.Search(len(pfPairs), func(i int) bool { return pfPairs[i] > uint64(uint32(p))<<32|0xFFFFFFFF })
		return pfPairs[lo:hi]
	}
	member := func(p, f chickpeas.NodeID) bool {
		return pfSet.Has(uint64(uint32(p))<<32 | uint64(uint32(f)))
	}
	// Distinct msg2 per person, counted via a flat (person, msg2) pair-set plus
	// a per-person counter incremented on first sight, rather than an inner map
	// per person: these inner sets were only read for their length.
	var seen flatset.U64Set
	counts := map[chickpeas.NodeID]int64{}
	for _, c := range cands {
		if c.p2 == c.p3 {
			continue
		}
		for _, key := range forumsOf(c.p2) {
			f1 := chickpeas.NodeID(uint32(key))
			if f1 == c.f2 || !member(c.p3, f1) {
				continue
			}
			for _, m1 := range m1Span(f1) {
				if c.ms2 > m1.ms1+deltaMS && !member(m1.p1, c.f2) {
					if seen.Add(uint64(uint32(m1.p1))<<32 | uint64(uint32(c.msg2))) {
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
	cells := make([]value.Value, len(ranked)*2)
	rows := make([][]value.Value, len(ranked))
	for i, c := range ranked {
		cells[i*2] = value.Int(c.id)
		cells[i*2+1] = value.Int(c.count)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}
