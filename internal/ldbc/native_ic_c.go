// Native IC complex-read kernels IC7-IC9 (recent likers, recent replies,
// recent friend/FoF messages). Split from native_ic_a.go for the file-size
// norm; the shared helpers (icPerson, msTop, the seed constants) and the
// init registration stay there.
package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// icIC7 -- the 20 most recent likers of the seed's messages (latest
// like per liker); [likeMs, likerId, isNew], (ms desc, id asc); isNew
// = not a direct friend (0/1).
func icIC7(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return nil, err
	}
	ldCol, ok := g.RelColIndexed("creationDate")
	if !ok {
		return nil, fmt.Errorf("rel column creationDate missing")
	}
	ld := ldCol.I64()
	return func() ([][]value.Value, error) {
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
		type cand struct{ ms, id, isNew int64 }
		cands := make([]cand, 0, len(best))
		for liker, rec := range best {
			isNew := int64(1)
			if friends[liker] {
				isNew = 0
			}
			cands = append(cands, cand{rec.ms, i64At(idCol, liker), isNew})
		}
		sortByLess(cands, func(a, b cand) bool {
			return cmpChain(cmpI64Desc(a.ms, b.ms), cmpI64Asc(a.id, b.id))
		})
		if len(cands) > 20 {
			cands = cands[:20]
		}
		cells := make([]value.Value, len(cands)*3)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*3] = value.Int(c.ms)
			cells[i*3+1] = value.Int(c.id)
			cells[i*3+2] = value.Int(c.isNew)
			rows[i] = cells[i*3 : i*3+3 : i*3+3]
		}
		return rows, nil
	}, nil
}

// icIC8 -- the 20 most recent replies to the seed's messages;
// [replyMs] (ms desc, reply id asc).
func icIC8(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := icPerson(g)
	if err != nil {
		return nil, err
	}
	msCol, err := nodeI64Col(g, "ms")
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
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
func icIC9(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
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
