// Native IC interactive-short kernels IS1-IS7 (profile, recent messages,
// friends, message content/creator, seed post forum/moderator, post
// replies). Split from native_ic_b.go for the file-size norm; the init
// registration and the IC10-14 complex kernels stay there.
package ldbc

import (
	"fmt"
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

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
		// Sort the raw ids and flat-back the rows: a slice per row cost one
		// allocation per friend.
		var ids []int64
		for f := range g.Neighbors(person, chickpeas.Both, "KNOWS") {
			ids = append(ids, i64At(idCol, f))
		}
		slices.Sort(ids)
		cells := make([]value.Value, len(ids))
		rows := make([][]value.Value, len(ids))
		for i, id := range ids {
			cells[i] = value.Int(id)
			rows[i] = cells[i : i+1 : i+1]
		}
		return rows, nil
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
