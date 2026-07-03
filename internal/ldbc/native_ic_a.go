// Native IC kernels IC1-IC9 -- ports of rustychickpeas-ldbc
// src/interactive/queries.rs onto the canonical .rcpg schema. The
// substitution parameters are the recorded seeds the refs were emitted
// with (python/refs/ic/seeds.json): the max-knows-degree person, a late
// date window, and derived tag/country/class anchors. The mirror
// schema's bidirectional lowercase `knows` becomes canonical
// single-stored KNOWS traversed Both; `person -hasCreator-> message`
// becomes the message-side HAS_CREATOR read from the person as
// Incoming.

package ldbc

import (
	"fmt"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
)

// Recorded IC seeds (python/refs/ic/seeds.json) -- the exact values the
// reference rows were emitted with.
const (
	icSeedPersonID  = 4398046519825
	icSeedPersonBID = 15393162798503
	icSeedFirstName = "John"
	icSeedMaxDay    = 15706 // days(2013-01-01)
	icIC4Start      = 14975 // days(2011-01-01)
	icIC4Dur        = 365
	icSeedTag       = "Augustine_of_Hippo"
	icSeedCountry   = "Indonesia"
	icSeedClass     = "Saint"
	icIS4MessageID  = 2336466638639
)

func init() {
	registerNative("IC", "IC1", icIC1)
	registerNative("IC", "IC2", icIC2)
	registerNative("IC", "IC3", icIC3)
	registerNative("IC", "IC4", icIC4)
	registerNative("IC", "IC5", icIC5)
	registerNative("IC", "IC6", icIC6)
	registerNative("IC", "IC7", icIC7)
	registerNative("IC", "IC8", icIC8)
	registerNative("IC", "IC9", icIC9)
}

// icPerson resolves the seed person (untimed prepare work, like the
// Rust harness's seed pick outside its timer).
func icPerson(g *chickpeas.Snapshot) (chickpeas.NodeID, error) {
	p, ok := nodeByID(g, "Person", icSeedPersonID)
	if !ok {
		return 0, fmt.Errorf("seed person %d missing", icSeedPersonID)
	}
	return p, nil
}

// msItem is one (creationMs, node) candidate of a most-recent-messages
// query.
type msItem struct {
	ms int64
	id chickpeas.NodeID
}

// msTop keeps the k best candidates by (ms desc, id asc) -- the
// streaming top-k the IC "most recent" reads share; the id tie-break
// only decides selection, the emitted rows carry ms alone.
type msTop struct {
	k     int
	items []msItem
}

func (t *msTop) push(ms int64, id chickpeas.NodeID) {
	n := len(t.items)
	if n == t.k {
		last := t.items[n-1]
		if ms < last.ms || (ms == last.ms && id >= last.id) {
			return
		}
	}
	i := sort.Search(n, func(i int) bool {
		it := t.items[i]
		return ms > it.ms || (ms == it.ms && id < it.id)
	})
	t.items = append(t.items, msItem{})
	copy(t.items[i+1:], t.items[i:])
	t.items[i] = msItem{ms, id}
	if len(t.items) > t.k {
		t.items = t.items[:t.k]
	}
}

// msRows renders the kept stamps as single-column rows.
func (t *msTop) msRows() [][]any {
	rows := make([][]any, len(t.items))
	for i, it := range t.items {
		rows[i] = []any{it.ms}
	}
	return rows
}

// icIC1 -- friends within 3 knows hops named John; [distance, lastName,
// personId], (distance, lastName, id) asc, top 20.
func icIC1(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		dist := g.BFSDistances(person, chickpeas.Both, g.Match("KNOWS"), 3)
		var rows [][]any
		for p, d := range dist {
			if d >= 1 && strAt(g, p, "firstName") == icSeedFirstName {
				rows = append(rows, []any{int64(d), strAt(g, p, "lastName"), i64At(idCol, p)})
			}
		}
		return sortTruncate(rows, 20, func(a, b []any) bool {
			return cmpChain(
				cmpI64Asc(a[0].(int64), b[0].(int64)),
				cmpStrAsc(a[1].(string), b[1].(string)),
				cmpI64Asc(a[2].(int64), b[2].(int64)),
			)
		}), nil
	}, nil
}

// icIC2 -- the 20 most recent messages by direct friends on/before
// maxDay; [creationMs] (ms desc, message id asc).
func icIC2(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		top := msTop{k: 20}
		for friend := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			for msg := range g.Neighbors(friend, chickpeas.Incoming, "HAS_CREATOR") {
				if i64At(dayCol, msg) > icSeedMaxDay {
					continue
				}
				top.push(i64At(msCol, msg), msg)
			}
		}
		return top.msRows(), nil
	}, nil
}

// icIC3 -- friends/FoF (not living in either country) with messages
// located in China AND Germany within a 1500-day window from
// 2010-01-01; [personId, xCount, yCount], (x+y desc, id asc), top 20.
func icIC3(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		cx, ok1 := nodeByName(g, "Country", "China")
		cy, ok2 := nodeByName(g, "Country", "Germany")
		if !ok1 || !ok2 {
			return [][]any{}, nil
		}
		startDay := dayFromCivil(2010, 1, 1)
		endDay := startDay + 1500
		reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 1, 2)
		var rows [][]any
		for p := range reach.Iter() {
			if home, ok := g.Follow(p,
				chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_LOCATED_IN"},
				chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_PART_OF"},
			); ok && (home == cx || home == cy) {
				continue
			}
			var xc, yc int64
			for msg := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
				day := i64At(dayCol, msg)
				if day < startDay || day >= endDay {
					continue
				}
				if c, ok := g.FirstNeighbor(msg, chickpeas.Outgoing, "IS_LOCATED_IN"); ok {
					switch c {
					case cx:
						xc++
					case cy:
						yc++
					}
				}
			}
			if xc > 0 && yc > 0 {
				rows = append(rows, []any{i64At(idCol, p), xc, yc})
			}
		}
		return sortTruncate(rows, 20, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[1].(int64)+a[2].(int64), b[1].(int64)+b[2].(int64)),
				cmpI64Asc(a[0].(int64), b[0].(int64)),
			)
		}), nil
	}, nil
}

// icIC4 -- new topics: tags on friends' Posts inside the window that
// never appeared on their Posts before it; [tagName, postCount],
// (count desc, name asc), top 10.
func icIC4(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		endDay := int64(icIC4Start + icIC4Dur)
		posts, _ := g.NodesWithLabel("Post")
		inWindow := map[chickpeas.NodeID]int64{}
		before := map[chickpeas.NodeID]bool{}
		for friend := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			for post := range g.Neighbors(friend, chickpeas.Incoming, "HAS_CREATOR") {
				if posts == nil || !posts.Contains(post) {
					continue
				}
				day := i64At(dayCol, post)
				if day < icIC4Start {
					for t := range g.Neighbors(post, chickpeas.Outgoing, "HAS_TAG") {
						before[t] = true
					}
				} else if day < endDay {
					for t := range g.Neighbors(post, chickpeas.Outgoing, "HAS_TAG") {
						inWindow[t]++
					}
				}
			}
		}
		var rows [][]any
		for t, c := range inWindow {
			if !before[t] {
				rows = append(rows, []any{strAt(g, t, "name"), c})
			}
		}
		return sortTruncate(rows, 10, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[1].(int64), b[1].(int64)),
				cmpStrAsc(a[0].(string), b[0].(string)),
			)
		}), nil
	}, nil
}

// icIC5 -- new groups: forums the friends/FoF joined after 2011-01-01,
// ranked by those members' Posts in each forum; [forumId, postCount],
// (count desc, id asc), top 20.
func icIC5(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	jdCol, ok := g.RelCol("joinDate")
	if !ok {
		return nil, fmt.Errorf("rel column joinDate missing")
	}
	jd := jdCol.I64()
	return func() ([][]any, error) {
		minDay := dayFromCivil(2011, 1, 1)
		reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 1, 2)
		forumCounts := map[chickpeas.NodeID]int64{}
		qforums := map[chickpeas.NodeID]bool{}
		for p := range reach.Iter() {
			clear(qforums)
			for e := range g.Rels(p, chickpeas.Incoming, "HAS_MEMBER") {
				if ms, ok := jd.Get(e.Pos); ok && floorDiv(ms, msPerDay) > minDay {
					qforums[e.Neighbor] = true
				}
			}
			if len(qforums) == 0 {
				continue
			}
			for post := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
				if forum, ok := g.FirstNeighbor(post, chickpeas.Incoming, "CONTAINER_OF"); ok && qforums[forum] {
					forumCounts[forum]++
				}
			}
		}
		rows := make([][]any, 0, len(forumCounts))
		for f, c := range forumCounts {
			rows = append(rows, []any{i64At(idCol, f), c})
		}
		return sortTruncate(rows, 20, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[1].(int64), b[1].(int64)),
				cmpI64Asc(a[0].(int64), b[0].(int64)),
			)
		}), nil
	}, nil
}

// icIC6 -- tag co-occurrence on friends/FoF Posts carrying the seed
// tag; [tagName, postCount], (count desc, name asc), top 10. The hot
// loops live in icIC6Rows: a named function compiles the nested
// traversal ranges allocation-free, which a closure body does not.
func icIC6(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) { return icIC6Rows(g, person) }, nil
}

// icIC6Rows is icIC6's query body.
func icIC6Rows(g *chickpeas.Snapshot, person chickpeas.NodeID) ([][]any, error) {
	target, ok := nodeByName(g, "Tag", icSeedTag)
	if !ok {
		return [][]any{}, nil
	}
	posts, _ := g.NodesWithLabel("Post")
	reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 1, 2)
	counts := map[chickpeas.NodeID]int64{}
	var tags []chickpeas.NodeID
	for p := range reach.Iter() {
		for post := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
			if posts == nil || !posts.Contains(post) {
				continue
			}
			tags = tags[:0]
			hasTarget := false
			for t := range g.Neighbors(post, chickpeas.Outgoing, "HAS_TAG") {
				if t == target {
					hasTarget = true
				}
				tags = append(tags, t)
			}
			if !hasTarget {
				continue
			}
			for _, t := range tags {
				if t != target {
					counts[t]++
				}
			}
		}
	}
	rows := make([][]any, 0, len(counts))
	for t, c := range counts {
		rows = append(rows, []any{strAt(g, t, "name"), c})
	}
	return sortTruncate(rows, 10, func(a, b []any) bool {
		return cmpChain(
			cmpI64Desc(a[1].(int64), b[1].(int64)),
			cmpStrAsc(a[0].(string), b[0].(string)),
		)
	}), nil
}

// icIC7 -- the 20 most recent likers of the seed's messages (latest
// like per liker); [likeMs, likerId, isNew], (ms desc, id asc); isNew
// = not a direct friend (0/1).
func icIC7(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	ldCol, ok := g.RelCol("creationDate")
	if !ok {
		return nil, fmt.Errorf("rel column creationDate missing")
	}
	ld := ldCol.I64()
	return func() ([][]any, error) {
		friends := map[chickpeas.NodeID]bool{}
		for f := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			friends[f] = true
		}
		type likeRec struct {
			ms  int64
			msg chickpeas.NodeID
		}
		best := map[chickpeas.NodeID]likeRec{}
		for msg := range g.Neighbors(person, chickpeas.Incoming, "HAS_CREATOR") {
			for e := range g.Rels(msg, chickpeas.Incoming, "LIKES") {
				lms, _ := ld.Get(e.Pos)
				cur, seen := best[e.Neighbor]
				if !seen || lms > cur.ms || (lms == cur.ms && msg < cur.msg) {
					best[e.Neighbor] = likeRec{lms, msg}
				}
			}
		}
		rows := make([][]any, 0, len(best))
		for liker, rec := range best {
			isNew := int64(1)
			if friends[liker] {
				isNew = 0
			}
			rows = append(rows, []any{rec.ms, i64At(idCol, liker), isNew})
		}
		return sortTruncate(rows, 20, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[0].(int64), b[0].(int64)),
				cmpI64Asc(a[1].(int64), b[1].(int64)),
			)
		}), nil
	}, nil
}

// icIC8 -- the 20 most recent replies to the seed's messages;
// [replyMs] (ms desc, reply id asc).
func icIC8(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		top := msTop{k: 20}
		for msg := range g.Neighbors(person, chickpeas.Incoming, "HAS_CREATOR") {
			for reply := range g.Neighbors(msg, chickpeas.Incoming, "REPLY_OF") {
				top.push(i64At(msCol, reply), reply)
			}
		}
		return top.msRows(), nil
	}, nil
}

// icIC9 -- the 20 most recent messages by friends and FoF on/before
// maxDay; [creationMs] (ms desc, message id asc).
func icIC9(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	dayCol, err := nodeI64Col(g, "day")
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 1, 2)
		top := msTop{k: 20}
		for p := range reach.Iter() {
			for msg := range g.Neighbors(p, chickpeas.Incoming, "HAS_CREATOR") {
				if i64At(dayCol, msg) > icSeedMaxDay {
					continue
				}
				top.push(i64At(msCol, msg), msg)
			}
		}
		return top.msRows(), nil
	}, nil
}
