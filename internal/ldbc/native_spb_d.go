// Native SPB kernels, advanced a20-a25 -- ports of rustychickpeas-ldbc
// src/spb/{a20..a25}.rs: the full-text ranked retrieval (a20) and
// faceted drill-downs (a21/a22/a23) over the core FullTextField, and
// the entity relatedness time-line (a24) / related-entities (a25)
// aggregates. Facet parameters bound by the parity run: a21 pins
// category+audience; a22 adds the dateCreated window; a23 is the final
// drill-down grouping distinct creation days per topic.

package ldbc

import (
	"fmt"
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
)

func init() {
	registerNative("SPB", "a20", simpleKernel(spbA20))
	registerNative("SPB", "a21", simpleKernel(spbA21))
	registerNative("SPB", "a22", simpleKernel(spbA22))
	registerNative("SPB", "a23", simpleKernel(spbA23))
	registerNative("SPB", "a24", simpleKernel(spbA24))
	registerNative("SPB", "a25", simpleKernel(spbA25))
}

// spbA20 (advanced q20, full-text): works whose title OR description
// matches the word (whole-word index union), ranked by dateModified
// descending.
func spbA20(g *chickpeas.Snapshot) ([][]any, error) {
	hits := g.FullTextSearch("CreativeWork", "description", spbWord).
		Or(g.FullTextSearch("CreativeWork", "title", spbWord))
	var rows []spbDated
	for w := range hits.Iter() {
		if dm, ok := g.Prop(w, "dateModified").Str(); ok {
			rows = append(rows, spbDated{w, dm})
		}
	}
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbA21 (advanced q21, faceted search): title full-text works with at
// least one about/mentions link, category pinned to the parameter
// category and audience to the parameter audience; deduped, id-ordered.
func spbA21(g *chickpeas.Snapshot) ([][]any, error) {
	var out []chickpeas.NodeID
	for w := range g.FullTextSearch("CreativeWork", "title", spbWord).Iter() {
		if !spbHasRel(g, w, "about") && !spbHasRel(g, w, "mentions") {
			continue
		}
		if !spbHasNeighborWithURI(g, w, "category", spbCategory) ||
			!spbHasNeighborWithURI(g, w, "audience", spbAudience) {
			continue
		}
		out = append(out, w)
	}
	slices.Sort(out)
	return spbURIRows(g, out), nil
}

// spbA22 (advanced q22, faceted full-text): title matches with the full
// BGP bound (description, liveCoverage, primaryFormat, about|mentions)
// plus the pinned category/audience facets and the inclusive
// dateCreated window; id-ordered.
func spbA22(g *chickpeas.Snapshot) ([][]any, error) {
	var out []chickpeas.NodeID
	for w := range g.FullTextSearch("CreativeWork", "title", spbWord).Iter() {
		created, ok := g.Prop(w, "dateCreated").Str()
		if !ok || created < spbDateFrom || created > spbDateTo {
			continue
		}
		if _, ok := g.Prop(w, "description").Str(); !ok {
			continue
		}
		if _, ok := g.Prop(w, "liveCoverage").Value(); !ok {
			continue
		}
		if !spbHasRel(g, w, "primaryFormat") {
			continue
		}
		if !spbHasNeighborWithURI(g, w, "category", spbCategory) ||
			!spbHasNeighborWithURI(g, w, "audience", spbAudience) {
			continue
		}
		if !spbHasRel(g, w, "about") && !spbHasRel(g, w, "mentions") {
			continue
		}
		out = append(out, w)
	}
	slices.Sort(out)
	return spbURIRows(g, out), nil
}

// spbA23 (advanced q23, final drill-down): per about/mentions topic of
// the title-matching, category-pinned works with the BGP bound, the
// count of distinct dateCreated calendar days; [uri, days] count
// descending then uri.
func spbA23(g *chickpeas.Snapshot) ([][]any, error) {
	byTag := map[chickpeas.NodeID]map[int64]bool{}
	for w := range g.FullTextSearch("CreativeWork", "title", spbWord).Iter() {
		if !spbHasNeighborWithURI(g, w, "category", spbCategory) {
			continue
		}
		created, ok := g.Prop(w, "dateCreated").Str()
		if !ok {
			continue
		}
		if _, ok := g.Prop(w, "description").Str(); !ok {
			continue
		}
		if !spbHasRel(g, w, "audience") || !spbHasRel(g, w, "primaryFormat") {
			continue
		}
		if _, ok := g.Prop(w, "liveCoverage").Value(); !ok {
			continue
		}
		if len(created) < 10 {
			continue
		}
		y, okY := spbAtoi(created[0:4])
		m, okM := spbAtoi(created[5:7])
		d, okD := spbAtoi(created[8:10])
		if !okY || !okM || !okD {
			continue
		}
		day := dayFromCivil(y, m, d)
		for _, pred := range spbTagPreds {
			for t := range g.Neighbors(w, chickpeas.Outgoing, pred) {
				set := byTag[t]
				if set == nil {
					set = map[int64]bool{}
					byTag[t] = set
				}
				set[day] = true
			}
		}
	}
	rows := make([][]any, 0, len(byTag))
	for t, days := range byTag {
		rows = append(rows, []any{spbURIOf(g, t), int64(len(days))})
	}
	spbSortKV(rows)
	return rows, nil
}

// spbA24 (advanced q24, relatedness time-line): per calendar day, the
// count of works about BOTH entities, ["YYYY-MM-DD", count] ascending.
func spbA24(g *chickpeas.Snapshot) ([][]any, error) {
	a, okA := spbNodeByURI(g, spbTopic)
	b, okB := spbNodeByURI(g, spbEntB)
	if !okA || !okB {
		return [][]any{}, nil
	}
	aboutA := map[chickpeas.NodeID]bool{}
	for w := range g.Neighbors(a, chickpeas.Incoming, "about") {
		aboutA[w] = true
	}
	both := map[chickpeas.NodeID]bool{}
	for w := range g.Neighbors(b, chickpeas.Incoming, "about") {
		if aboutA[w] && g.HasLabel(w, "CreativeWork") {
			both[w] = true
		}
	}
	perDay := map[string]int64{}
	for w := range both {
		created, ok := g.Prop(w, "dateCreated").Str()
		if !ok || len(created) < 10 {
			continue
		}
		y, okY := spbAtoi(created[0:4])
		m, okM := spbAtoi(created[5:7])
		d, okD := spbAtoi(created[8:10])
		if !okY || !okM || !okD {
			continue
		}
		perDay[fmt.Sprintf("%04d-%02d-%02d", y, m, d)]++
	}
	rows := make([][]any, 0, len(perDay))
	for day, n := range perDay {
		rows = append(rows, []any{day, n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0].(string) < rows[j][0].(string) })
	return rows, nil
}

// spbA25 (advanced q25, related entities): entities co-occurring with
// the topic in works' about links, counted by distinct dateCreated
// days; [who-uri, days] ordered days descending then node id.
func spbA25(g *chickpeas.Snapshot) ([][]any, error) {
	a, ok := spbNodeByURI(g, spbTopic)
	if !ok {
		return [][]any{}, nil
	}
	days := map[chickpeas.NodeID]map[string]bool{}
	for cw := range g.Neighbors(a, chickpeas.Incoming, "about") {
		if !g.HasLabel(cw, "CreativeWork") {
			continue
		}
		created, ok := g.Prop(cw, "dateCreated").Str()
		if !ok || len(created) < 10 {
			continue
		}
		day := created[:10]
		for who := range g.Neighbors(cw, chickpeas.Outgoing, "about") {
			if who == a {
				continue
			}
			set := days[who]
			if set == nil {
				set = map[string]bool{}
				days[who] = set
			}
			set[day] = true
		}
	}
	type row struct {
		who chickpeas.NodeID
		n   int64
	}
	rows := make([]row, 0, len(days))
	for who, set := range days {
		rows = append(rows, row{who, int64(len(set))})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].who < rows[j].who
	})
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = []any{spbURIOf(g, r.who), r.n}
	}
	return out, nil
}
