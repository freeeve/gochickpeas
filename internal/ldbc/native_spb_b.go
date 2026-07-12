// Native SPB kernels, advanced a1-a10 -- ports of rustychickpeas-ldbc
// src/spb/{a1..a10}.rs. Counting queries deliberately do NOT dedup
// parallel rels: the Rust kernels count per rel statement (and their
// hand-adapted SPARQL oracle unions about/mentions as a multiset), so a
// work tagging one topic through both predicates counts twice wherever
// the Rust counts it twice (a5/a6/a7/a8), and once where the Rust
// dedups (q5's per-work topic set).

package ldbc

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/nodeset"
)

func init() {
	registerNative("SPB", "a1", simpleKernel(spbA1))
	registerNative("SPB", "a2", spbA2)
	registerNative("SPB", "a3", simpleKernel(spbA3))
	registerNative("SPB", "a4", simpleKernel(spbA4))
	registerNative("SPB", "a5", simpleKernel(spbA5))
	registerNative("SPB", "a6", simpleKernel(spbA6))
	registerNative("SPB", "a7", simpleKernel(spbA7))
	registerNative("SPB", "a8", simpleKernel(spbA8))
	registerNative("SPB", "a9", simpleKernel(spbA9))
	registerNative("SPB", "a10", simpleKernel(spbA10))
}

// spbSubtypes are the concrete CreativeWork subclasses the SPB data
// instantiates.
var spbSubtypes = []string{"BlogPost", "NewsItem", "Programme"}

// spbOutCount is a node's outgoing rel count of one type, duplicates
// included (the Rust neighbors_by_type().count()).
func spbOutCount(g *chickpeas.Snapshot, n chickpeas.NodeID, rel string) int64 {
	var c int64
	for range g.Neighbors(n, chickpeas.Outgoing, rel) {
		c++
	}
	return c
}

// spbCountRows renders a node-keyed histogram as [uri, count] rows,
// count descending then uri ascending.
func spbCountRows(g *chickpeas.Snapshot, counts map[chickpeas.NodeID]int64) [][]any {
	rows := make([][]any, 0, len(counts))
	for n, c := range counts {
		rows = append(rows, []any{spbURIOf(g, n), c})
	}
	spbSortKV(rows)
	return rows
}

// spbA1 (advanced q1): creative works with an `about` rel to the topic
// and a dateModified, newest first.
func spbA1(g *chickpeas.Snapshot) ([][]any, error) {
	topic, ok := spbNodeByURI(g, spbTopic)
	if !ok {
		return [][]any{}, nil
	}
	cworks, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	var rows []spbDated
	for w := range g.Neighbors(topic, chickpeas.Incoming, "about") {
		if !cworks.Contains(w) {
			continue
		}
		if dt, ok := g.Prop(w, "dateModified").Str(); ok {
			rows = append(rows, spbDated{w, dt})
		}
	}
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbA2 (advanced q2): the derived creative work's CreativeWork subtype
// labels, sorted; empty for a missing or titleless work. The parameter
// derivation is untimed prepare.
func spbA2(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cwURI := spbQ2CW(g)
	return func() ([][]any, error) {
		work, ok := spbNodeByURI(g, cwURI)
		if !ok || !g.HasLabel(work, "CreativeWork") {
			return [][]any{}, nil
		}
		if _, hasTitle := g.Prop(work, "title").Str(); !hasTitle {
			return [][]any{}, nil
		}
		var rows [][]any
		for _, label := range spbSubtypes {
			if g.HasLabel(work, label) {
				rows = append(rows, []any{label})
			}
		}
		return rows, nil
	}, nil
}

// spbA3 (advanced q3): in-window works counted by the minute-of-hour of
// dateModified (the SPARQL MINUTES), minute emitted as a bare decimal
// string.
func spbA3(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	counts := map[int64]int64{}
	for w := range works.Iter() {
		dt, ok := g.Prop(w, "dateModified").Str()
		if !ok || !(dt > spbDateFrom && dt < spbDateTo) {
			continue
		}
		if len(dt) < 16 {
			continue
		}
		minute, ok := spbAtoi(dt[14:16])
		if !ok {
			continue
		}
		counts[minute]++
	}
	rows := make([][]any, 0, len(counts))
	for m, c := range counts {
		rows = append(rows, []any{strconvItoa(m), c})
	}
	spbSortKV(rows)
	return rows, nil
}

// strconvItoa formats a small non-negative int64 (a3's minute buckets)
// without pulling fmt into the hot path.
func strconvItoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// spbA4 (advanced q4): works per concrete subtype with dateModified in
// the window, count descending.
func spbA4(g *chickpeas.Snapshot) ([][]any, error) {
	var rows [][]any
	for _, label := range spbSubtypes {
		set, ok := g.NodesWithLabel(label)
		if !ok {
			continue
		}
		var n int64
		for w := range set.Iter() {
			if dm, ok := g.Prop(w, "dateModified").Str(); ok && dm > spbDateFrom && dm < spbDateTo {
				n++
			}
		}
		if n > 0 {
			rows = append(rows, []any{label, n})
		}
	}
	spbSortKV(rows)
	return rows, nil
}

// spbA5 (advanced q5): about-targets of the entity type ranked by how
// many works category-linked to either pinned category are about them
// (per about rel, duplicates included).
func spbA5(g *chickpeas.Snapshot) ([][]any, error) {
	entities, ok := g.NodesWithLabel(spbEntityLabel)
	if !ok {
		return [][]any{}, nil
	}
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	counts := map[chickpeas.NodeID]int64{}
	for w := range works.Iter() {
		inCategory := false
		for c := range g.Neighbors(w, chickpeas.Outgoing, "category") {
			if u, ok := g.Prop(c, "uri").Str(); ok && (u == spbCatCompany || u == spbCategory) {
				inCategory = true
				break
			}
		}
		if !inCategory {
			continue
		}
		for about := range g.Neighbors(w, chickpeas.Outgoing, "about") {
			if entities.Contains(about) {
				counts[about]++
			}
		}
	}
	return spbCountRows(g, counts), nil
}

// spbA6 (advanced q6): about-entity types (leaf classes plus the
// forward-chained Thing) ranked by covered (work, about) pairs, over
// works with the live flag and audience.
func spbA6(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	entityTypes := []string{"Company", "Event", "Thing"}
	type typeSet struct {
		name string
		set  *nodeset.Set
	}
	var sets []typeSet
	for _, ty := range entityTypes {
		if s, ok := g.NodesWithLabel(ty); ok {
			sets = append(sets, typeSet{ty, s})
		}
	}
	counts := map[string]int64{}
	for w := range works.Iter() {
		if live, ok := g.Prop(w, "liveCoverage").Bool(); !ok || !live {
			continue
		}
		if !spbHasNeighborWithURI(g, w, "audience", spbAudience) {
			continue
		}
		for about := range g.Neighbors(w, chickpeas.Outgoing, "about") {
			for _, ts := range sets {
				if ts.set.Contains(about) {
					counts[ts.name]++
				}
			}
		}
	}
	rows := make([][]any, 0, len(counts))
	for ty, n := range counts {
		rows = append(rows, []any{ty, n})
	}
	spbSortKV(rows)
	return rows, nil
}

// spbA7 (advanced q7): mention targets ranked by mentions from works
// whose primaryContentOf out-degree exceeds the threshold (1).
func spbA7(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	counts := map[chickpeas.NodeID]int64{}
	for w := range works.Iter() {
		if spbOutCount(g, w, "primaryContentOf") <= 1 {
			continue
		}
		for m := range g.Neighbors(w, chickpeas.Outgoing, "mentions") {
			counts[m]++
		}
	}
	return spbCountRows(g, counts), nil
}

// spbA8 (advanced q8): topics ranked by tag rels (the loader's
// materialized about/mentions super-property, counted per rel) from
// works of the type/audience inside the dateModified window.
func spbA8(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel(spbCWType)
	if !ok {
		return [][]any{}, nil
	}
	counts := map[chickpeas.NodeID]int64{}
	for w := range works.Iter() {
		dt, ok := g.Prop(w, "dateModified").Str()
		if !ok || !(dt > spbDateFrom && dt < spbDateTo) {
			continue
		}
		if !spbHasNeighborWithURI(g, w, "audience", spbAudience) {
			continue
		}
		for t := range g.Neighbors(w, chickpeas.Outgoing, "tag") {
			counts[t]++
		}
	}
	return spbCountRows(g, counts), nil
}

// spbA9 (advanced q9): the largest outgoing mentions count on any
// single creative work, as the one-row [max, n] aggregate the parity
// ref stores.
func spbA9(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{{"max", int64(0)}}, nil
	}
	var maxN int64
	for w := range works.Iter() {
		if n := spbOutCount(g, w, "mentions"); n > maxN {
			maxN = n
		}
	}
	return [][]any{{"max", maxN}}, nil
}

// spbA10 (advanced q10): every dateCreated-carrying work whose mentions
// count attains the global maximum, as [uri, count] ordered by uri.
func spbA10(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	type wc struct {
		id chickpeas.NodeID
		n  int64
	}
	var counts []wc
	var maxN int64
	for w := range works.Iter() {
		n := spbOutCount(g, w, "mentions")
		counts = append(counts, wc{w, n})
		if n > maxN {
			maxN = n
		}
	}
	if maxN == 0 {
		return [][]any{}, nil
	}
	var rows [][]any
	for _, c := range counts {
		if c.n != maxN {
			continue
		}
		if _, ok := g.Prop(c.id, "dateCreated").Str(); !ok {
			continue
		}
		rows = append(rows, []any{spbURIOf(g, c.id), c.n})
	}
	sortByLess(rows, func(a, b []any) bool { return a[0].(string) < b[0].(string) })
	return rows, nil
}
