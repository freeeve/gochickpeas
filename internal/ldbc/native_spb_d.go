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

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

func init() {
	registerNativeV("SPB", "a20", simpleKernelV(spbA20))
	registerNativeV("SPB", "a21", simpleKernelV(spbA21))
	registerNativeV("SPB", "a22", simpleKernelV(spbA22))
	registerNativeV("SPB", "a23", simpleKernelV(spbA23))
	registerNative("SPB", "a24", simpleKernel(spbA24))
	registerNativeV("SPB", "a25", simpleKernelV(spbA25))
}

// spbA20 (advanced q20, full-text): works whose title OR description
// matches the word (whole-word index union), ranked by dateModified
// descending.
func spbA20(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
func spbA21(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
func spbA22(g *chickpeas.Snapshot) ([][]value.Value, error) {
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
func spbA23(g *chickpeas.Snapshot) ([][]value.Value, error) {
	// Distinct calendar days per tag, held in a flat (tag, day) pair-set plus a
	// per-tag counter bumped on first sight rather than a map-of-maps: the inner
	// day sets were only read for their length, so one flat set avoids an inner
	// map per tag.
	type tagDay struct {
		t   chickpeas.NodeID
		day int64
	}
	seen := map[tagDay]bool{}
	counts := map[chickpeas.NodeID]int64{}
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
				pair := tagDay{t, day}
				if !seen[pair] {
					seen[pair] = true
					counts[t]++
				}
			}
		}
	}
	cells := make([]value.Value, len(counts)*2)
	rows := make([][]value.Value, 0, len(counts))
	i := 0
	for t, c := range counts {
		cells[i*2] = value.Str(spbURIOf(g, t))
		cells[i*2+1] = value.Int(c)
		rows = append(rows, cells[i*2:i*2+2:i*2+2])
		i++
	}
	spbSortKVV(rows)
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
	sortByLess(rows, func(a, b []any) bool { return a[0].(string) < b[0].(string) })
	return rows, nil
}

// spbA25 (advanced q25, related entities): entities co-occurring with
// the topic in works' about links, counted by distinct dateCreated
// days; [who-uri, days] ordered days descending then node id.
func spbA25(g *chickpeas.Snapshot) ([][]value.Value, error) {
	a, ok := spbNodeByURI(g, spbTopic)
	if !ok {
		return [][]value.Value{}, nil
	}
	// Distinct dateCreated days per co-occurring entity, held in a flat
	// (who, day) pair-set plus a per-entity counter bumped on first sight
	// rather than a map-of-maps: the inner day sets were only read for their
	// length, so one flat set avoids an inner map per entity.
	type whoDay struct {
		who chickpeas.NodeID
		day string
	}
	seen := map[whoDay]bool{}
	counts := map[chickpeas.NodeID]int64{}
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
			pair := whoDay{who, day}
			if !seen[pair] {
				seen[pair] = true
				counts[who]++
			}
		}
	}
	type row struct {
		who chickpeas.NodeID
		n   int64
	}
	rows := make([]row, 0, len(counts))
	for who, c := range counts {
		rows = append(rows, row{who, c})
	}
	sortByLess(rows, func(a, b row) bool {
		if a.n != b.n {
			return a.n > b.n
		}
		return a.who < b.who
	})
	// Zero-box result: a pre-sized flat backing carved into two-cell row views,
	// so the whole result is a small constant number of allocations.
	cells := make([]value.Value, len(rows)*2)
	out := make([][]value.Value, len(rows))
	for i, r := range rows {
		cells[i*2] = value.Str(spbURIOf(g, r.who))
		cells[i*2+1] = value.Int(r.n)
		out[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return out, nil
}
