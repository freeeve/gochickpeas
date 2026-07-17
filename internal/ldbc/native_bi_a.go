// Native BI kernels Q1, Q2, Q5-Q9, Q11, Q12 -- ports of
// rustychickpeas-ldbc src/bi/faithful_a.rs onto the canonical .rcpg
// schema, emitting rows in the committed refs' column order
// (src/bi/emit.rs). Official bi-*.cypher params are inlined, matching
// the refs.

package ldbc

import (
	"fmt"
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
	"github.com/freeeve/gochickpeas/internal/parallel"
)

func init() {
	registerNativeV("BI", "Q1", simpleKernelV(biQ1))
	registerNativeV("BI", "Q2", simpleKernelV(biQ2))
	registerNativeV("BI", "Q5", simpleKernelV(biQ5))
	registerNativeV("BI", "Q6", simpleKernelV(biQ6))
	registerNativeV("BI", "Q7", simpleKernelV(biQ7))
	registerNativeV("BI", "Q8", simpleKernelV(biQ8))
	registerNativeV("BI", "Q9", simpleKernelV(biQ9))
	registerNativeV("BI", "Q11", simpleKernelV(biQ11))
	registerNativeV("BI", "Q12", biQ12)
}

// biQ1 -- posting summary. Group messages before the cutoff (with
// content) by (year, isComment, lengthCategory); official RETURN order
// [year, isComment, messageCount, avg, sum, pct] with pct over ALL
// pre-cutoff messages.
func biQ1(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
	contentCol, ok := g.ColIndexed("content")
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
	rows := make([][]value.Value, 0, len(groups))
	for k, v := range groups {
		avg := float64(v[1]) / float64(v[0])
		pct := float64(v[0]) / float64(total)
		rows = append(rows, []value.Value{
			value.Int(k.year), value.Bool(k.isComment), value.Int(k.cat),
			value.Int(v[0]), value.Float(avg), value.Int(v[1]), value.Float(pct),
		})
	}
	return sortTruncate(rows, 0, func(a, b []value.Value) bool {
		ay, _ := a[0].AsInt()
		by, _ := b[0].AsInt()
		ab, _ := a[1].AsBool()
		bb, _ := b[1].AsBool()
		ac, bc := int64(0), int64(0)
		if ab {
			ac = 1
		}
		if bb {
			bc = 1
		}
		acat, _ := a[2].AsInt()
		bcat, _ := b[2].AsInt()
		return cmpChain(
			cmpI64Desc(ay, by),
			cmpI64Asc(ac, bc),
			cmpI64Asc(acat, bcat),
		)
	}), nil
}

// biQ2 -- tag evolution. Messages tagged with MusicalArtist tags in two
// consecutive 100-day windows from 2012-06-01; [name, w1, w2, |diff|],
// diff desc / name asc, top 100. Every qualifying tag emits a row.
func biQ2(g *chickpeas.Snapshot) ([][]value.Value, error) {
	date0 := dayFromCivil(2012, 6, 1)
	target, ok := nodeByName(g, "TagClass", "MusicalArtist")
	if !ok {
		return [][]value.Value{}, nil
	}
	// The qualifying tags are a small fixed set: index them once (sorted
	// ids, position = dense index) so the parallel shards count into flat
	// per-tag slabs and merge by vector add -- the per-shard count maps
	// paid bucket growth on every shard.
	var tags []uint32
	for t := range g.Neighbors(target, chickpeas.Incoming, "HAS_TYPE") {
		tags = append(tags, uint32(t))
	}
	slices.Sort(tags)
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	w1lo, w1hi := date0, date0+100
	w2lo, w2hi := date0+100, date0+200
	type winCounts struct{ c1, c2 []int64 }
	c1 := make([]int64, len(tags))
	c2 := make([]int64, len(tags))
	for _, label := range []string{"Post", "Comment"} {
		set, ok := g.NodesWithLabel(label)
		if !ok {
			continue
		}
		ids := set.ToSlice()
		part := parallel.Fold(len(ids),
			func() winCounts {
				return winCounts{make([]int64, len(tags)), make([]int64, len(tags))}
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
					if ti, ok := slices.BinarySearch(tags, uint32(t)); ok {
						if in1 {
							acc.c1[ti]++
						} else {
							acc.c2[ti]++
						}
					}
				}
				return acc
			},
			func(a, b winCounts) winCounts {
				for i := range b.c1 {
					a.c1[i] += b.c1[i]
				}
				for i := range b.c2 {
					a.c2[i] += b.c2[i]
				}
				return a
			})
		for i := range part.c1 {
			c1[i] += part.c1[i]
		}
		for i := range part.c2 {
			c2[i] += part.c2[i]
		}
	}
	type cand struct {
		name           string
		n1, n2, absDif int64
	}
	cands := make([]cand, 0, len(tags))
	for ti, t := range tags {
		n1, n2 := c1[ti], c2[ti]
		diff := n1 - n2
		if diff < 0 {
			diff = -diff
		}
		cands = append(cands, cand{strAt(g, chickpeas.NodeID(t), "name"), n1, n2, diff})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.absDif, b.absDif), cmpStrAsc(a.name, b.name))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	cells := make([]value.Value, len(cands)*4)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*4] = value.Str(c.name)
		cells[i*4+1] = value.Int(c.n1)
		cells[i*4+2] = value.Int(c.n2)
		cells[i*4+3] = value.Int(c.absDif)
		rows[i] = cells[i*4 : i*4+4 : i*4+4]
	}
	return rows, nil
}

// biQ5 -- most active posters of a topic (Abbas_I_of_Persia). Score
// creators of tagged messages by 1*msgs + 2*replies + 10*likes;
// [personId, replyCount, likeCount, messageCount, score], top 100.
func biQ5(g *chickpeas.Snapshot) ([][]value.Value, error) {
	target, ok := nodeByName(g, "Tag", "Abbas_I_of_Persia")
	if !ok {
		return [][]value.Value{}, nil
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
	type cand struct{ id, r, l, m, score int64 }
	cands := make([]cand, 0, len(agg))
	for p, s := range agg {
		cands = append(cands, cand{i64At(idCol, p), s.r, s.l, s.m, s.m + 2*s.r + 10*s.l})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.score, b.score), cmpI64Asc(a.id, b.id))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	cells := make([]value.Value, len(cands)*5)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*5] = value.Int(c.id)
		cells[i*5+1] = value.Int(c.r)
		cells[i*5+2] = value.Int(c.l)
		cells[i*5+3] = value.Int(c.m)
		cells[i*5+4] = value.Int(c.score)
		rows[i] = cells[i*5 : i*5+5 : i*5+5]
	}
	return rows, nil
}

// biQ6 -- most authoritative users on a topic (Arnold_Schwarzenegger).
// authority(person1) = sum over distinct likers of person1's tagged
// messages of the likes those likers received on their own messages;
// [personId, authorityScore], top 100.
func biQ6(g *chickpeas.Snapshot) ([][]value.Value, error) {
	target, ok := nodeByName(g, "Tag", "Arnold_Schwarzenegger")
	if !ok {
		return [][]value.Value{}, nil
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	// Distinct (author, liker) pairs, held in a flat pair-set rather than a
	// map-of-maps: the inner sets were only iterated (to enumerate distinct
	// likers and sum their scores), never probed for membership, so one flat
	// dedup set avoids allocating an inner map per author.
	type authorLiker struct{ p1, p2 chickpeas.NodeID }
	seen := map[authorLiker]bool{}
	var likers []chickpeas.NodeID // message's liker list, reused per message
	for m1 := range g.Neighbors(target, chickpeas.Incoming, "HAS_TAG") {
		likers = likers[:0]
		for liker := range g.Neighbors(m1, chickpeas.Incoming, "LIKES") {
			likers = append(likers, liker)
		}
		if len(likers) == 0 {
			continue
		}
		for p1 := range g.Neighbors(m1, chickpeas.Outgoing, "HAS_CREATOR") {
			for _, l := range likers {
				seen[authorLiker{p1, l}] = true
			}
		}
	}
	// One pass over the distinct pairs: score each liker lazily (once) and
	// accumulate it into its author's authority sum.
	likerScore := map[chickpeas.NodeID]int64{}
	sum := map[chickpeas.NodeID]int64{}
	for pair := range seen {
		sc, done := likerScore[pair.p2]
		if !done {
			for m2 := range g.Neighbors(pair.p2, chickpeas.Incoming, "HAS_CREATOR") {
				for range g.Neighbors(m2, chickpeas.Incoming, "LIKES") {
					sc++
				}
			}
			likerScore[pair.p2] = sc
		}
		sum[pair.p1] += sc
	}
	type cand struct{ id, score int64 }
	cands := make([]cand, 0, len(sum))
	for p1, s := range sum {
		cands = append(cands, cand{i64At(idCol, p1), s})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.score, b.score), cmpI64Asc(a.id, b.id))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	cells := make([]value.Value, len(cands)*2)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*2] = value.Int(c.id)
		cells[i*2+1] = value.Int(c.score)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}

// biQ7 -- related topics (Enrique_Iglesias). Distinct comments replying
// to tagged messages, not themselves tagged, counted per other tag;
// [name, count], count desc / name asc, top 100.
func biQ7(g *chickpeas.Snapshot) ([][]value.Value, error) {
	target, ok := nodeByName(g, "Tag", "Enrique_Iglesias")
	if !ok {
		return [][]value.Value{}, nil
	}
	// Distinct comments per related tag, counted via a flat (tag, comment)
	// pair-set rather than a map-of-maps: the inner sets were only read for
	// their length, so one flat dedup set plus a per-tag counter incremented
	// on first sight avoids allocating an inner map per co-occurring tag.
	type rtComment struct{ rt, comment chickpeas.NodeID }
	seen := map[rtComment]bool{}
	counts := map[chickpeas.NodeID]int64{}
	var ctags []chickpeas.NodeID // comment's tag list, reused per comment
	for msg := range g.Neighbors(target, chickpeas.Incoming, "HAS_TAG") {
		for comment := range g.Neighbors(msg, chickpeas.Incoming, "REPLY_OF") {
			ctags = ctags[:0]
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
				pair := rtComment{rt, comment}
				if !seen[pair] {
					seen[pair] = true
					counts[rt]++
				}
			}
		}
	}
	// Typed rows, boxing only the top 100 -- counts spans every co-occurring
	// tag, far more than the output.
	type cand struct {
		name  string
		count int64
	}
	cands := make([]cand, 0, len(counts))
	for rt, c := range counts {
		cands = append(cands, cand{strAt(g, rt, "name"), c})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.count, b.count), cmpStrAsc(a.name, b.name))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	cells := make([]value.Value, len(cands)*2)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*2] = value.Str(c.name)
		cells[i*2+1] = value.Int(c.count)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}

// biQ8 -- central person for a tag (Che_Guevara, 2011-07-20..25
// exclusive). score = 100*interest + tagged messages in window;
// friendsScore sums friends' scores; [personId, score, friendsScore],
// (score+friendsScore) desc / id asc, top 100.
func biQ8(g *chickpeas.Snapshot) ([][]value.Value, error) {
	tag, ok := nodeByName(g, "Tag", "Che_Guevara")
	if !ok {
		return [][]value.Value{}, nil
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
	rows := make([][]value.Value, 0, len(score))
	for p, s := range score {
		var fs int64
		for f := range g.Neighbors(p, chickpeas.Both, "KNOWS") {
			fs += score[f]
		}
		rows = append(rows, []value.Value{value.Int(i64At(idCol, p)), value.Int(s), value.Int(fs)})
	}
	return sortTruncate(rows, 100, func(a, b []value.Value) bool {
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

// biQ9 -- top thread initiators (2011-10-01..15 inclusive). Per person:
// posts in window and messages in those posts' reply trees (in window,
// pruned past endDay); [personId, threadCount, messageCount],
// messageCount desc / id asc, top 100.
func biQ9(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
		var stack []chickpeas.NodeID // reply-tree DFS scratch, reused per post
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
			stack = append(stack[:0], post)
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
	// Typed rows, sorted/truncated typed, boxing only the top 100 -- perPerson
	// spans every in-window thread initiator, far more than the output.
	type cand struct{ id, threads, msgs int64 }
	cands := make([]cand, 0, len(perPerson))
	for p, e := range perPerson {
		cands = append(cands, cand{i64At(idCol, p), e.threads, e.msgs})
	}
	sortByLess(cands, func(a, b cand) bool {
		return cmpChain(cmpI64Desc(a.msgs, b.msgs), cmpI64Asc(a.id, b.id))
	})
	if len(cands) > 100 {
		cands = cands[:100]
	}
	cells := make([]value.Value, len(cands)*3)
	rows := make([][]value.Value, len(cands))
	for i, c := range cands {
		cells[i*3] = value.Int(c.id)
		cells[i*3+1] = value.Int(c.threads)
		cells[i*3+2] = value.Int(c.msgs)
		rows[i] = cells[i*3 : i*3+3 : i*3+3]
	}
	return rows, nil
}

// biQ11 -- friend triangles (India, 2012-09-29..2013-01-01 inclusive).
// Count knows-triangles among persons of the country where every edge
// was created in the window; [[count]].
func biQ11(g *chickpeas.Snapshot) ([][]value.Value, error) {
	country, ok := nodeByName(g, "Country", "India")
	if !ok {
		return [][]value.Value{}, nil
	}
	startDay, endDay := dayFromCivil(2012, 9, 29), dayFromCivil(2013, 1, 1)
	kdCol, ok := g.RelColIndexed("creationDate")
	if !ok {
		return nil, fmt.Errorf("rel column creationDate missing")
	}
	kd := kdCol.I64()
	inList, inSet := personsOfCountryFlat(g, country)
	// The date-window KNOWS subgraph as one sorted (a<<32|b) edge-key slice
	// (per-node spans) plus a flat probe set -- the map-of-maps form
	// allocated an inner set per person with a qualifying edge.
	var edges []uint64
	var edgeSet flatset.U64Set
	for _, a := range inList {
		for e := range g.Rels(a, chickpeas.Both, "KNOWS") {
			if !inSet.Has(uint32(e.Neighbor)) {
				continue
			}
			ms, ok := kd.Get(e.Pos)
			if !ok {
				continue
			}
			day := floorDiv(ms, msPerDay)
			if day >= startDay && day <= endDay {
				key := uint64(uint32(a))<<32 | uint64(uint32(e.Neighbor))
				if edgeSet.Add(key) {
					edges = append(edges, key)
				}
			}
		}
	}
	slices.Sort(edges)
	nbrSpan := func(p uint32) []uint64 {
		lo := sort.Search(len(edges), func(i int) bool { return edges[i] >= uint64(p)<<32 })
		hi := sort.Search(len(edges), func(i int) bool { return edges[i] > uint64(p)<<32|0xFFFFFFFF })
		return edges[lo:hi]
	}
	var count int64
	for _, ab := range edges {
		a, b := uint32(ab>>32), uint32(ab)
		if b <= a {
			continue
		}
		for _, bc := range nbrSpan(b) {
			if c := uint32(bc); c > b && edgeSet.Has(uint64(a)<<32|uint64(c)) {
				count++
			}
		}
	}
	return [][]value.Value{{value.Int(count)}}, nil
}

// biQ12 -- message-count histogram (len<20, after 2010-07-22, root
// language ar/hu). Persons histogrammed by qualifying-message count
// (zero bucket included); [messageCount, personCount], personCount desc
// / messageCount desc. Prepare-form kernel: the column handles, label id
// slices, and the FoldInto worker accumulators live in the untimed
// prepare and are cleared per run, so a warm run's fold allocates
// nothing for accumulator state (borrowed-accumulator pattern).
func biQ12(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
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
	contentCol, ok := g.ColIndexed("content")
	if !ok {
		return nil, fmt.Errorf("node column content missing")
	}
	contentStr := contentCol.Str()
	langCol, ok := g.ColIndexed("language")
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
	var labelIDs [][]chickpeas.NodeID
	for _, label := range []string{"Post", "Comment"} {
		if set, ok := g.NodesWithLabel(label); ok {
			labelIDs = append(labelIDs, set.ToSlice())
		}
	}
	totalPersons := int64(0)
	if ps, ok := g.NodesWithLabel("Person"); ok {
		totalPersons = int64(ps.Len())
	}
	accs := make([]map[chickpeas.NodeID]int64, parallel.Workers())
	for i := range accs {
		accs[i] = map[chickpeas.NodeID]int64{}
	}
	counts := map[chickpeas.NodeID]int64{}
	hist := map[int64]int64{}
	return func() ([][]value.Value, error) {
		clear(counts)
		for _, ids := range labelIDs {
			for _, m := range accs {
				clear(m)
			}
			part := parallel.FoldInto(accs, len(ids),
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
		clear(hist)
		for _, c := range counts {
			hist[c]++
		}
		hist[0] = totalPersons - int64(len(counts))
		type hrow struct{ mc, pc int64 }
		ranked := make([]hrow, 0, len(hist))
		for mc, pc := range hist {
			ranked = append(ranked, hrow{mc, pc})
		}
		sortByLess(ranked, func(a, b hrow) bool {
			return cmpChain(cmpI64Desc(a.pc, b.pc), cmpI64Desc(a.mc, b.mc))
		})
		cells := make([]value.Value, len(ranked)*2)
		rows := make([][]value.Value, len(ranked))
		for i, r := range ranked {
			cells[i*2] = value.Int(r.mc)
			cells[i*2+1] = value.Int(r.pc)
			rows[i] = cells[i*2 : i*2+2 : i*2+2]
		}
		return rows, nil
	}, nil
}
