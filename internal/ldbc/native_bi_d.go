// Native BI kernel Q17 (information propagation). Split from
// native_bi_c.go for the file-size norm; the init registration and the
// other BI kernels (Q3/Q4/Q10/Q15/Q16) stay there.
package ldbc

import (
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/flatset"
)

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
