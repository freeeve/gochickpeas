// Native IC kernels IC10-IC14 and short reads IS1-IS7 -- ports of
// rustychickpeas-ldbc src/interactive/queries.rs (continued). The IS
// reads anchor on the seed's newest Post / most recent message,
// resolved in the untimed prepare phase like the Rust harness's ctx.

package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

func init() {
	registerNativeV("IC", "IC10", icIC10)
	registerNativeV("IC", "IC11", icIC11)
	registerNativeV("IC", "IC12", icIC12)
	registerNativeV("IC", "IC13", icIC13)
	registerNativeV("IC", "IC14", icIC14)
	registerNativeV("IC", "IS1", icIS1)
	registerNativeV("IC", "IS2", icIS2)
	registerNativeV("IC", "IS3", icIS3)
	registerNativeV("IC", "IS4", icIS4)
	registerNativeV("IC", "IS5", icIS5)
	registerNativeV("IC", "IS6", icIS6)
	registerNativeV("IC", "IS7", icIS7)
}

// icIC10 -- friend recommendation (month 1): FoF at exactly 2 hops born
// in [Jan 21, Feb 22), scored by tagged-vs-untagged Posts against the
// seed's interests; [personId, score], (score desc, id asc), top 10.
func icIC10(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	bmonCol, err := nodeI64Col(g, "bmon")
	if err != nil {
		return nil, err
	}
	bdomCol, err := nodeI64Col(g, "bdom")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		const month = 1
		next := int64(month%12 + 1)
		interests := map[chickpeas.NodeID]bool{}
		for t := range g.Neighbors(person, chickpeas.Outgoing, "HAS_INTEREST") {
			interests[t] = true
		}
		posts, _ := g.NodesWithLabel("Post")
		reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 2, 2)
		type cand struct{ id, score int64 }
		var cands []cand
		for foaf := range reach.Iter() {
			bmon, bdom := i64At(bmonCol, foaf), i64At(bdomCol, foaf)
			if !((bmon == month && bdom >= 21) || (bmon == next && bdom < 22)) {
				continue
			}
			var common, uncommon int64
			for msg := range g.Neighbors(foaf, chickpeas.Incoming, "HAS_CREATOR") {
				if posts == nil || !posts.Contains(msg) {
					continue
				}
				tagged := false
				for t := range g.Neighbors(msg, chickpeas.Outgoing, "HAS_TAG") {
					if interests[t] {
						tagged = true
						break
					}
				}
				if tagged {
					common++
				} else {
					uncommon++
				}
			}
			cands = append(cands, cand{i64At(idCol, foaf), common - uncommon})
		}
		sortByLess(cands, func(a, b cand) bool {
			return cmpChain(cmpI64Desc(a.score, b.score), cmpI64Asc(a.id, b.id))
		})
		if len(cands) > 10 {
			cands = cands[:10]
		}
		cells := make([]value.Value, len(cands)*2)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*2] = value.Int(c.id)
			cells[i*2+1] = value.Int(c.score)
			rows[i] = cells[i*2 : i*2+2 : i*2+2]
		}
		return rows, nil
	}, nil
}

// icIC11 -- job referral (Indonesia, workFrom < 2030): the <=2-hop
// neighbourhood's work records at companies of the country; [personId,
// companyName, workFrom], (workFrom asc, id asc, name desc), top 10.
func icIC11(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	wfCol, ok := g.RelColIndexed("workFrom")
	if !ok {
		return nil, fmt.Errorf("rel column workFrom missing")
	}
	wf := wfCol.I64()
	return func() ([][]value.Value, error) {
		const year = 2030
		country, ok := nodeByName(g, "Country", icSeedCountry)
		if !ok {
			return [][]value.Value{}, nil
		}
		places := map[chickpeas.NodeID]bool{country: true}
		for city := range g.Neighbors(country, chickpeas.Incoming, "IS_PART_OF") {
			places[city] = true
		}
		inCountry := map[chickpeas.NodeID]bool{}
		if comps, ok := g.NodesWithLabel("Company"); ok {
			for org := range comps.Iter() {
				for pl := range g.Neighbors(org, chickpeas.Outgoing, "IS_LOCATED_IN") {
					if places[pl] {
						inCountry[org] = true
						break
					}
				}
			}
		}
		reach := g.Neighborhood(person, chickpeas.Both, g.Match("KNOWS"), 1, 2)
		type cand struct {
			id, from int64
			name     string
		}
		var cands []cand
		for p := range reach.Iter() {
			for e := range g.Rels(p, chickpeas.Outgoing, "WORK_AT") {
				if !inCountry[e.Neighbor] {
					continue
				}
				from, ok := wf.Get(e.Pos)
				if !ok || from >= year {
					continue
				}
				cands = append(cands, cand{i64At(idCol, p), from, strAt(g, e.Neighbor, "name")})
			}
		}
		sortByLess(cands, func(a, b cand) bool {
			return cmpChain(
				cmpI64Asc(a.from, b.from),
				cmpI64Asc(a.id, b.id),
				-cmpStrAsc(a.name, b.name),
			)
		})
		if len(cands) > 10 {
			cands = cands[:10]
		}
		cells := make([]value.Value, len(cands)*3)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*3] = value.Int(c.id)
			cells[i*3+1] = value.Str(c.name)
			cells[i*3+2] = value.Int(c.from)
			rows[i] = cells[i*3 : i*3+3 : i*3+3]
		}
		return rows, nil
	}, nil
}

// icIC12 -- expert search (TagClass Saint + descendants): direct
// friends by replies to Posts tagged under the class; [personId,
// replyCount], (count desc, id asc), top 20.
func icIC12(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		rootClass, ok := nodeByName(g, "TagClass", icSeedClass)
		if !ok {
			return [][]value.Value{}, nil
		}
		classSet := g.BFSDistances(rootClass, chickpeas.Incoming, g.Match("IS_SUBCLASS_OF"), chickpeas.NoMaxDepth)
		qualTag := func(t chickpeas.NodeID) bool {
			for c := range g.Neighbors(t, chickpeas.Outgoing, "HAS_TYPE") {
				if _, in := classSet[c]; in {
					return true
				}
			}
			return false
		}
		posts, _ := g.NodesWithLabel("Post")
		type cand struct{ id, count int64 }
		var cands []cand
		for friend := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			var count int64
			for c := range g.Neighbors(friend, chickpeas.Incoming, "HAS_CREATOR") {
				for parent := range g.Neighbors(c, chickpeas.Outgoing, "REPLY_OF") {
					if posts == nil || !posts.Contains(parent) {
						continue
					}
					matched := false
					for t := range g.Neighbors(parent, chickpeas.Outgoing, "HAS_TAG") {
						if qualTag(t) {
							matched = true
						}
					}
					if matched {
						count++
					}
				}
			}
			if count > 0 {
				cands = append(cands, cand{i64At(idCol, friend), count})
			}
		}
		sortByLess(cands, func(a, b cand) bool {
			return cmpChain(cmpI64Desc(a.count, b.count), cmpI64Asc(a.id, b.id))
		})
		if len(cands) > 20 {
			cands = cands[:20]
		}
		cells := make([]value.Value, len(cands)*2)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*2] = value.Int(c.id)
			cells[i*2+1] = value.Int(c.count)
			rows[i] = cells[i*2 : i*2+2 : i*2+2]
		}
		return rows, nil
	}, nil
}

// icPersonB resolves the recorded far-reachable seed person_b.
func icPersonB(g *chickpeas.Snapshot) (chickpeas.NodeID, error) {
	p, ok := nodeByID(g, "Person", icSeedPersonBID)
	if !ok {
		return 0, fmt.Errorf("seed person_b %d missing", icSeedPersonBID)
	}
	return p, nil
}

// icIC13 -- unweighted shortest knows-path length between the seeds;
// [[hops]] (-1 when unreachable).
func icIC13(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	personB, err := icPersonB(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		unit := func(chickpeas.NodeID, chickpeas.RelRef) float64 { return 1.0 }
		if cost, ok := g.WeightedShortestPath(person, personB, chickpeas.Both, g.Match("KNOWS"), unit); ok {
			return [][]value.Value{{value.Int(int64(cost))}}, nil
		}
		return [][]value.Value{{value.Int(int64(-1))}}, nil
	}, nil
}

// icIC14 -- weighted shortest knows-path cost between the seeds, edge
// cost 1/(reply interactions + 1); [[cost]] or [] when unreachable.
// The interaction map is built in the untimed prepare phase (the Rust
// harness times only the search).
func icIC14(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	personB, err := icPersonB(g)
	if err != nil {
		return nil, err
	}
	interaction := buildInteractionMap(g)
	return func() ([][]value.Value, error) {
		weight := func(from chickpeas.NodeID, rel chickpeas.RelRef) float64 {
			return 1.0 / (float64(interaction[pairKey(from, rel.Neighbor)]) + 1.0)
		}
		if cost, ok := g.WeightedShortestPath(person, personB, chickpeas.Both, g.Match("KNOWS"), weight); ok && finite(cost) {
			return [][]value.Value{{value.Float(cost)}}, nil
		}
		return [][]value.Value{}, nil
	}, nil
}

// icIS1 -- the seed's profile; [[firstName, lastName]].
func icIS1(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		return [][]value.Value{{value.Str(strAt(g, person, "firstName")), value.Str(strAt(g, person, "lastName"))}}, nil
	}, nil
}

// is2Top computes the seed's own most recent messages on/before maxDay
// -- shared by IS2 (rows) and IS5's anchor derivation.
func is2Top(g *chickpeas.Snapshot, person chickpeas.NodeID, dayCol, msCol chickpeas.I64Col) msTop {
	top := msTop{k: 10}
	for m := range g.Neighbors(person, chickpeas.Incoming, "HAS_CREATOR") {
		if i64At(dayCol, m) > icSeedMaxDay {
			continue
		}
		top.push(i64At(msCol, m), m)
	}
	return top
}

// icIS2 -- the seed's own 10 most recent messages on/before maxDay;
// [creationMs] (ms desc, id asc).
func icIS2(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
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
	return func() ([][]value.Value, error) {
		top := is2Top(g, person, dayCol, msCol)
		return top.msRows(), nil
	}, nil
}

// icIS3 -- the seed's direct friends; [personId] ascending.
func icIS3(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		var rows [][]value.Value
		for f := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			rows = append(rows, []value.Value{value.Int(i64At(idCol, f))})
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			return a0 < b0
		}), nil
	}, nil
}

// icIS4 -- the recorded seed message's (creationMs, content); the
// message id is pinned by the refs' seeds.json.
func icIS4(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	msg, ok := nodeByID(g, "Message", icIS4MessageID)
	if !ok {
		return nil, fmt.Errorf("seed message %d missing", icIS4MessageID)
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		content, ok := g.Prop(msg, "content").Str()
		if !ok {
			return nil, fmt.Errorf("message %d has no content", icIS4MessageID)
		}
		return [][]value.Value{{value.Int(i64At(msCol, msg)), value.Str(content)}}, nil
	}, nil
}

// icIS5 -- the creator of the seed's most recent message (the IS2 top
// row, resolved in prepare like the Rust harness); [[personId]].
func icIS5(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
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
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	top := is2Top(g, person, dayCol, msCol)
	if len(top.items) == 0 {
		return nil, fmt.Errorf("seed person has no messages for IS5")
	}
	msg := top.items[0].id
	return func() ([][]value.Value, error) {
		if creator, ok := creatorOf(g, msg); ok {
			return [][]value.Value{{value.Int(i64At(idCol, creator))}}, nil
		}
		return [][]value.Value{{value.Int(int64(-1))}}, nil
	}, nil
}

// icSeedPost resolves the seed's newest Post (max creationMs; ties by
// smallest LDBC id) -- the IS6/IS7 anchor the Rust ctx derives once.
func icSeedPost(g *chickpeas.Snapshot) (chickpeas.NodeID, error) {
	person, err := icPerson(g)
	if err != nil {
		return 0, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return 0, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return 0, err
	}
	posts, ok := g.NodesWithLabel("Post")
	if !ok {
		return 0, fmt.Errorf("label Post missing")
	}
	var best chickpeas.NodeID
	bestMS, found := int64(-1), false
	for m := range g.Neighbors(person, chickpeas.Incoming, "HAS_CREATOR") {
		if !posts.Contains(m) {
			continue
		}
		ms := i64At(msCol, m)
		if !found || ms > bestMS || (ms == bestMS && i64At(idCol, m) < i64At(idCol, best)) {
			best, bestMS, found = m, ms, true
		}
	}
	if !found {
		return 0, fmt.Errorf("seed person has no Posts for IS6/IS7")
	}
	return best, nil
}

// icIS6 -- the forum and moderator of the seed post; [[forumId,
// moderatorId]].
func icIS6(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	post, err := icSeedPost(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	var roots chickpeas.RootsVia
	if rt, ok := g.RelType("REPLY_OF"); ok {
		roots = g.RootsVia(rt, chickpeas.Outgoing)
	}
	return func() ([][]value.Value, error) {
		root := post
		if roots != nil {
			root = roots[post]
		}
		forum, ok := g.FirstNeighbor(root, chickpeas.Incoming, "CONTAINER_OF")
		if !ok {
			return [][]value.Value{}, nil
		}
		moderator, ok := g.FirstNeighbor(forum, chickpeas.Outgoing, "HAS_MODERATOR")
		if !ok {
			return [][]value.Value{}, nil
		}
		return [][]value.Value{{value.Int(i64At(idCol, forum)), value.Int(i64At(idCol, moderator))}}, nil
	}, nil
}

// icIS7 -- direct replies to the seed post; [replyMs, authorId, knows]
// (ms desc, author id asc); knows = author is a friend of the post's
// author (0/1).
func icIS7(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	post, err := icSeedPost(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		author, hasAuthor := creatorOf(g, post)
		authorFriends := map[chickpeas.NodeID]bool{}
		if hasAuthor {
			for f := range g.Neighbors(author, chickpeas.Both, "KNOWS") {
				authorFriends[f] = true
			}
		}
		var rows [][]value.Value
		for reply := range g.Neighbors(post, chickpeas.Incoming, "REPLY_OF") {
			ra, ok := creatorOf(g, reply)
			knows := int64(0)
			raID := int64(0)
			if ok {
				raID = i64At(idCol, ra)
				if hasAuthor && ra != author && authorFriends[ra] {
					knows = 1
				}
			}
			rows = append(rows, []value.Value{value.Int(i64At(msCol, reply)), value.Int(raID), value.Int(knows)})
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			a1, _ := a[1].AsInt()
			b1, _ := b[1].AsInt()
			return cmpChain(
				cmpI64Desc(a0, b0),
				cmpI64Asc(a1, b1),
			)
		}), nil
	}, nil
}
