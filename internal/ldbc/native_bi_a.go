// Native BI kernels Q1, Q2, Q5-Q9, Q11, Q12 -- ports of
// rustychickpeas-ldbc src/bi/faithful_a.rs onto the canonical .rcpg
// schema, emitting rows in the committed refs' column order
// (src/bi/emit.rs). Official bi-*.cypher params are inlined, matching
// the refs.

package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/parallel"
)

func init() {
	registerNative("BI", "Q1", simpleKernel(biQ1))
	registerNative("BI", "Q2", simpleKernel(biQ2))
	registerNative("BI", "Q5", simpleKernel(biQ5))
	registerNative("BI", "Q6", simpleKernel(biQ6))
	registerNative("BI", "Q7", simpleKernel(biQ7))
	registerNative("BI", "Q8", simpleKernel(biQ8))
	registerNative("BI", "Q9", simpleKernel(biQ9))
	registerNative("BI", "Q11", simpleKernel(biQ11))
	registerNative("BI", "Q12", simpleKernel(biQ12))
}

// biQ1 -- posting summary. Group messages before the cutoff (with
// content) by (year, isComment, lengthCategory); official RETURN order
// [year, isComment, messageCount, avg, sum, pct] with pct over ALL
// pre-cutoff messages.
func biQ1(g *chickpeas.Snapshot) ([][]any, error) {
	cutoff := dayFromCivil(2011, 12, 1)
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	lenCol, err := nodeI64Col(g, "length")
	if err != nil {
		return nil, err
	}
	yearCol, err := nodeI64Col(g, "year")
	if err != nil {
		return nil, err
	}
	contentCol, ok := g.Col("content")
	if !ok {
		return nil, fmt.Errorf("node column content missing")
	}
	contentStr := contentCol.Str()

	type key struct {
		year      int64
		isComment bool
		cat       int64
	}
	type agg struct {
		groups map[key][2]int64 // count, sumLength
		total  int64
	}
	total := int64(0)
	groups := map[key][2]int64{}
	for _, lc := range []struct {
		label     string
		isComment bool
	}{{"Post", false}, {"Comment", true}} {
		set, ok := g.NodesWithLabel(lc.label)
		if !ok {
			continue
		}
		ids := set.ToSlice()
		part := parallel.Fold(len(ids),
			func() agg { return agg{groups: map[key][2]int64{}} },
			func(acc agg, i int) agg {
				n := ids[i]
				if i64At(dayCol, n) >= cutoff {
					return acc
				}
				acc.total++
				if _, present := contentStr.ID(n); !present {
					return acc
				}
				l := i64At(lenCol, n)
				var cat int64
				switch {
				case l < 40:
					cat = 0
				case l < 80:
					cat = 1
				case l < 160:
					cat = 2
				default:
					cat = 3
				}
				k := key{i64At(yearCol, n), lc.isComment, cat}
				e := acc.groups[k]
				e[0]++
				e[1] += l
				acc.groups[k] = e
				return acc
			},
			func(a, b agg) agg {
				for k, v := range b.groups {
					e := a.groups[k]
					e[0] += v[0]
					e[1] += v[1]
					a.groups[k] = e
				}
				a.total += b.total
				return a
			})
		for k, v := range part.groups {
			e := groups[k]
			e[0] += v[0]
			e[1] += v[1]
			groups[k] = e
		}
		total += part.total
	}
	rows := make([][]any, 0, len(groups))
	for k, v := range groups {
		avg := float64(v[1]) / float64(v[0])
		pct := float64(v[0]) / float64(total)
		rows = append(rows, []any{k.year, k.isComment, k.cat, v[0], avg, v[1], pct})
	}
	return sortTruncate(rows, 0, func(a, b []any) bool {
		ac, bc := int64(0), int64(0)
		if a[1].(bool) {
			ac = 1
		}
		if b[1].(bool) {
			bc = 1
		}
		return cmpChain(
			cmpI64Desc(a[0].(int64), b[0].(int64)),
			cmpI64Asc(ac, bc),
			cmpI64Asc(a[2].(int64), b[2].(int64)),
		)
	}), nil
}

// biQ2 -- tag evolution. Messages tagged with MusicalArtist tags in two
// consecutive 100-day windows from 2012-06-01; [name, w1, w2, |diff|],
// diff desc / name asc, top 100. Every qualifying tag emits a row.
func biQ2(g *chickpeas.Snapshot) ([][]any, error) {
	date0 := dayFromCivil(2012, 6, 1)
	target, ok := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok {
		return [][]any{}, nil
	}
	qualifying := map[chickpeas.NodeID]bool{}
	for t := range g.Neighbors(target, chickpeas.Incoming, "HAS_TYPE") {
		qualifying[t] = true
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	w1lo, w1hi := date0, date0+100
	w2lo, w2hi := date0+100, date0+200
	type winCounts struct{ c1, c2 map[chickpeas.NodeID]int64 }
	c1 := map[chickpeas.NodeID]int64{}
	c2 := map[chickpeas.NodeID]int64{}
	for _, label := range []string{"Post", "Comment"} {
		set, ok := g.NodesWithLabel(label)
		if !ok {
			continue
		}
		ids := set.ToSlice()
		part := parallel.Fold(len(ids),
			func() winCounts {
				return winCounts{map[chickpeas.NodeID]int64{}, map[chickpeas.NodeID]int64{}}
			},
			func(acc winCounts, i int) winCounts {
				msg := ids[i]
				day := i64At(dayCol, msg)
				in1 := w1lo <= day && day < w1hi
				in2 := w2lo <= day && day < w2hi
				if !in1 && !in2 {
					return acc
				}
				for t := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_TAG") {
					if qualifying[t] {
						if in1 {
							acc.c1[t]++
						} else {
							acc.c2[t]++
						}
					}
				}
				return acc
			},
			func(a, b winCounts) winCounts {
				for k, v := range b.c1 {
					a.c1[k] += v
				}
				for k, v := range b.c2 {
					a.c2[k] += v
				}
				return a
			})
		for k, v := range part.c1 {
			c1[k] += v
		}
		for k, v := range part.c2 {
			c2[k] += v
		}
	}
	rows := make([][]any, 0, len(qualifying))
	for t := range qualifying {
		n1, n2 := c1[t], c2[t]
		diff := n1 - n2
		if diff < 0 {
			diff = -diff
		}
		rows = append(rows, []any{strAt(g, t, "name"), n1, n2, diff})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[3].(int64), b[3].(int64)),
			cmpStrAsc(a[0].(string), b[0].(string)),
		)
	}), nil
}

// biQ5 -- most active posters of a topic (Abbas_I_of_Persia). Score
// creators of tagged messages by 1*msgs + 2*replies + 10*likes;
// [personId, replyCount, likeCount, messageCount, score], top 100.
func biQ5(g *chickpeas.Snapshot) ([][]any, error) {
	target, ok := nodeByName(g, "Tag", "Abbas_I_of_Persia")
	if !ok {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	type score struct{ m, r, l int64 }
	agg := map[chickpeas.NodeID]score{}
	for msg := range g.Neighbors(target, chickpeas.Incoming, "HAS_TAG") {
		var likes, replies int64
		for range g.Neighbors(msg, chickpeas.Incoming, "LIKES") {
			likes++
		}
		for range g.Neighbors(msg, chickpeas.Incoming, "REPLY_OF") {
			replies++
		}
		for person := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_CREATOR") {
			e := agg[person]
			e.m++
			e.r += replies
			e.l += likes
			agg[person] = e
		}
	}
	rows := make([][]any, 0, len(agg))
	for p, s := range agg {
		rows = append(rows, []any{i64At(idCol, p), s.r, s.l, s.m, s.m + 2*s.r + 10*s.l})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[4].(int64), b[4].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ6 -- most authoritative users on a topic (Arnold_Schwarzenegger).
// authority(person1) = sum over distinct likers of person1's tagged
// messages of the likes those likers received on their own messages;
// [personId, authorityScore], top 100.
func biQ6(g *chickpeas.Snapshot) ([][]any, error) {
	target, ok := nodeByName(g, "Tag", "Arnold_Schwarzenegger")
	if !ok {
		return [][]any{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	p1ToP2 := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	for m1 := range g.Neighbors(target, chickpeas.Incoming, "HAS_TAG") {
		var likers []chickpeas.NodeID
		for liker := range g.Neighbors(m1, chickpeas.Incoming, "LIKES") {
			likers = append(likers, liker)
		}
		if len(likers) == 0 {
			continue
		}
		for p1 := range g.Neighbors(m1, chickpeas.Outgoing, "HAS_CREATOR") {
			set := p1ToP2[p1]
			if set == nil {
				set = map[chickpeas.NodeID]bool{}
				p1ToP2[p1] = set
			}
			for _, l := range likers {
				set[l] = true
			}
		}
	}
	likerScore := map[chickpeas.NodeID]int64{}
	for _, p2set := range p1ToP2 {
		for p2 := range p2set {
			if _, done := likerScore[p2]; done {
				continue
			}
			var s int64
			for m2 := range g.Neighbors(p2, chickpeas.Incoming, "HAS_CREATOR") {
				for range g.Neighbors(m2, chickpeas.Incoming, "LIKES") {
					s++
				}
			}
			likerScore[p2] = s
		}
	}
	rows := make([][]any, 0, len(p1ToP2))
	for p1, p2set := range p1ToP2 {
		var s int64
		for p2 := range p2set {
			s += likerScore[p2]
		}
		rows = append(rows, []any{i64At(idCol, p1), s})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64), b[1].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ7 -- related topics (Enrique_Iglesias). Distinct comments replying
// to tagged messages, not themselves tagged, counted per other tag;
// [name, count], count desc / name asc, top 100.
func biQ7(g *chickpeas.Snapshot) ([][]any, error) {
	target, ok := nodeByName(g, "Tag", "Enrique_Iglesias")
	if !ok {
		return [][]any{}, nil
	}
	related := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	for msg := range g.Neighbors(target, chickpeas.Incoming, "HAS_TAG") {
		for comment := range g.Neighbors(msg, chickpeas.Incoming, "REPLY_OF") {
			var ctags []chickpeas.NodeID
			hasTarget := false
			for t := range g.Neighbors(comment, chickpeas.Outgoing, "HAS_TAG") {
				if t == target {
					hasTarget = true
				}
				ctags = append(ctags, t)
			}
			if hasTarget {
				continue
			}
			for _, rt := range ctags {
				set := related[rt]
				if set == nil {
					set = map[chickpeas.NodeID]bool{}
					related[rt] = set
				}
				set[comment] = true
			}
		}
	}
	rows := make([][]any, 0, len(related))
	for rt, cs := range related {
		rows = append(rows, []any{strAt(g, rt, "name"), int64(len(cs))})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64), b[1].(int64)),
			cmpStrAsc(a[0].(string), b[0].(string)),
		)
	}), nil
}

// biQ8 -- central person for a tag (Che_Guevara, 2011-07-20..25
// exclusive). score = 100*interest + tagged messages in window;
// friendsScore sums friends' scores; [personId, score, friendsScore],
// (score+friendsScore) desc / id asc, top 100.
func biQ8(g *chickpeas.Snapshot) ([][]any, error) {
	tag, ok := nodeByName(g, "Tag", "Che_Guevara")
	if !ok {
		return [][]any{}, nil
	}
	startDay, endDay := dayFromCivil(2011, 7, 20), dayFromCivil(2011, 7, 25)
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	score := map[chickpeas.NodeID]int64{}
	for p := range g.Neighbors(tag, chickpeas.Incoming, "HAS_INTEREST") {
		score[p] += 100
	}
	for msg := range g.Neighbors(tag, chickpeas.Incoming, "HAS_TAG") {
		day := i64At(dayCol, msg)
		if day > startDay && day < endDay {
			for creator := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_CREATOR") {
				score[creator]++
			}
		}
	}
	rows := make([][]any, 0, len(score))
	for p, s := range score {
		var fs int64
		for f := range g.Neighbors(p, chickpeas.Both, "KNOWS") {
			fs += score[f]
		}
		rows = append(rows, []any{i64At(idCol, p), s, fs})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64)+a[2].(int64), b[1].(int64)+b[2].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ9 -- top thread initiators (2011-10-01..15 inclusive). Per person:
// posts in window and messages in those posts' reply trees (in window,
// pruned past endDay); [personId, threadCount, messageCount],
// messageCount desc / id asc, top 100.
func biQ9(g *chickpeas.Snapshot) ([][]any, error) {
	startDay, endDay := dayFromCivil(2011, 10, 1), dayFromCivil(2011, 10, 15)
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	type tm struct{ threads, msgs int64 }
	perPerson := map[chickpeas.NodeID]tm{}
	posts, ok := g.NodesWithLabel("Post")
	if ok {
		for post := range posts.Iter() {
			pd := i64At(dayCol, post)
			if pd < startDay || pd > endDay {
				continue
			}
			creator, ok := creatorOf(g, post)
			if !ok {
				continue
			}
			var msgs int64
			stack := []chickpeas.NodeID{post}
			for len(stack) > 0 {
				n := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				d := i64At(dayCol, n)
				if d > endDay {
					continue
				}
				if d >= startDay {
					msgs++
				}
				for c := range g.Neighbors(n, chickpeas.Incoming, "REPLY_OF") {
					stack = append(stack, c)
				}
			}
			e := perPerson[creator]
			e.threads++
			e.msgs += msgs
			perPerson[creator] = e
		}
	}
	rows := make([][]any, 0, len(perPerson))
	for p, e := range perPerson {
		rows = append(rows, []any{i64At(idCol, p), e.threads, e.msgs})
	}
	return sortTruncate(rows, 100, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[2].(int64), b[2].(int64)),
			cmpI64Asc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}

// biQ11 -- friend triangles (India, 2012-09-29..2013-01-01 inclusive).
// Count knows-triangles among persons of the country where every edge
// was created in the window; [[count]].
func biQ11(g *chickpeas.Snapshot) ([][]any, error) {
	country, ok := nodeByName(g, "Country", "India")
	if !ok {
		return [][]any{}, nil
	}
	startDay, endDay := dayFromCivil(2012, 9, 29), dayFromCivil(2013, 1, 1)
	kdCol, ok := g.RelCol("creationDate")
	if !ok {
		return nil, fmt.Errorf("rel column creationDate missing")
	}
	kd := kdCol.I64()
	inCountry := personsOfCountry(g, country)
	adj := map[chickpeas.NodeID]map[chickpeas.NodeID]bool{}
	for a := range inCountry {
		for e := range g.Rels(a, chickpeas.Both, "KNOWS") {
			if !inCountry[e.Neighbor] {
				continue
			}
			ms, ok := kd.Get(e.Pos)
			if !ok {
				continue
			}
			day := floorDiv(ms, msPerDay)
			if day >= startDay && day <= endDay {
				set := adj[a]
				if set == nil {
					set = map[chickpeas.NodeID]bool{}
					adj[a] = set
				}
				set[e.Neighbor] = true
			}
		}
	}
	var count int64
	for a, nbrsA := range adj {
		for b := range nbrsA {
			if b <= a {
				continue
			}
			for c := range adj[b] {
				if c > b && nbrsA[c] {
					count++
				}
			}
		}
	}
	return [][]any{{count}}, nil
}

// biQ12 -- message-count histogram (len<20, after 2010-07-22, root
// language ar/hu). Persons histogrammed by qualifying-message count
// (zero bucket included); [messageCount, personCount], personCount desc
// / messageCount desc.
func biQ12(g *chickpeas.Snapshot) ([][]any, error) {
	minDay := dayFromCivil(2010, 7, 22)
	const lenThr = 20
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	lenCol, err := nodeI64Col(g, "length")
	if err != nil {
		return nil, err
	}
	contentCol, ok := g.Col("content")
	if !ok {
		return nil, fmt.Errorf("node column content missing")
	}
	contentStr := contentCol.Str()
	langCol, ok := g.Col("language")
	if !ok {
		return nil, fmt.Errorf("node column language missing")
	}
	langStr := langCol.Str()
	langIDs := map[uint32]bool{}
	for _, l := range []string{"ar", "hu"} {
		if id, ok := g.Atoms().ID(l); ok {
			langIDs[id] = true
		}
	}
	var roots chickpeas.RootsVia
	if rt, ok := g.RelType("REPLY_OF"); ok {
		roots = g.RootsVia(rt, chickpeas.Outgoing)
	}
	rootOf := func(n chickpeas.NodeID) chickpeas.NodeID {
		if roots == nil {
			return n
		}
		return roots[n]
	}

	counts := map[chickpeas.NodeID]int64{}
	for _, label := range []string{"Post", "Comment"} {
		set, ok := g.NodesWithLabel(label)
		if !ok {
			continue
		}
		ids := set.ToSlice()
		part := parallel.Fold(len(ids),
			func() map[chickpeas.NodeID]int64 { return map[chickpeas.NodeID]int64{} },
			func(acc map[chickpeas.NodeID]int64, i int) map[chickpeas.NodeID]int64 {
				msg := ids[i]
				if i64At(dayCol, msg) <= minDay {
					return acc
				}
				if _, present := contentStr.ID(msg); !present {
					return acc
				}
				if i64At(lenCol, msg) >= lenThr {
					return acc
				}
				lid, ok := langStr.ID(rootOf(msg))
				if !ok || !langIDs[lid] {
					return acc
				}
				if creator, ok := creatorOf(g, msg); ok {
					acc[creator]++
				}
				return acc
			},
			func(a, b map[chickpeas.NodeID]int64) map[chickpeas.NodeID]int64 {
				for k, v := range b {
					a[k] += v
				}
				return a
			})
		for k, v := range part {
			counts[k] += v
		}
	}
	totalPersons := int64(0)
	if ps, ok := g.NodesWithLabel("Person"); ok {
		totalPersons = int64(ps.Len())
	}
	hist := map[int64]int64{}
	for _, c := range counts {
		hist[c]++
	}
	hist[0] = totalPersons - int64(len(counts))
	rows := make([][]any, 0, len(hist))
	for mc, pc := range hist {
		rows = append(rows, []any{mc, pc})
	}
	return sortTruncate(rows, 0, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64), b[1].(int64)),
			cmpI64Desc(a[0].(int64), b[0].(int64)),
		)
	}), nil
}
