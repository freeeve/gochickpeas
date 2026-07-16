// Native SPB kernels, advanced a13-a19 -- ports of rustychickpeas-ldbc
// src/spb/{a13..a19}.rs. a15/a16 run off the core full-text field over
// CreativeWork title (whole-word, like the Rust full_text_search); a17
// is the geo bounding-box drill-down over the Feature lat/long k-d
// index. Both indexes build lazily on first use, warmed by the parity
// pass exactly as the Rust harness's first run warms its core indexes.

package ldbc

import (
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

func init() {
	registerNativeV("SPB", "a13", simpleKernelV(spbA13))
	registerNative("SPB", "a14", simpleKernel(spbA14))
	registerNative("SPB", "a15", simpleKernel(spbA15))
	registerNative("SPB", "a16", simpleKernel(spbA16))
	registerNative("SPB", "a17", simpleKernel(spbA17))
	registerNative("SPB", "a18", simpleKernel(spbA18))
	registerNative("SPB", "a19", simpleKernel(spbA19))
}

// spbA13 (advanced q13): distinct (work, tag) pairs for works
// category-linked to either pinned category with a dateModified; tags
// without a uri (blank nodes) drop after the DISTINCT.
func spbA13(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
	}
	type pair struct{ w, t chickpeas.NodeID }
	var pairs []pair
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
		if _, ok := g.Prop(w, "dateModified").Str(); !ok {
			continue
		}
		for t := range g.Neighbors(w, chickpeas.Outgoing, "tag") {
			pairs = append(pairs, pair{w, t})
		}
	}
	sortByLess(pairs, func(a, b pair) bool {
		if a.w != b.w {
			return a.w < b.w
		}
		return a.t < b.t
	})
	// Zero-box result: two value.Str cells per surviving row appended into one
	// pre-sized flat backing (upper bound = every pair survives), then carved
	// into fixed-cap row views -- a small constant number of allocations
	// regardless of row count.
	cells := make([]value.Value, 0, len(pairs)*2)
	var last pair
	for i, p := range pairs {
		if i > 0 && p == last {
			continue // SELECT DISTINCT over (?thing, ?tag)
		}
		last = p
		if tagURI, ok := g.Prop(p.t, "uri").Str(); ok {
			cells = append(cells, value.Str(spbURIOf(g, p.w)), value.Str(tagURI))
		}
	}
	n := len(cells) / 2
	rows := make([][]value.Value, n)
	for i := range rows {
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}

// spbA14 (advanced q14): the full required star (tag, category,
// thumbnail, audience rels + dateModified) with primaryFormat pinned to
// the parameter node and a primaryContentOf web document of the pinned
// webDocumentType, newest first.
func spbA14(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	pf, okPF := spbNodeByURI(g, spbPrimaryFormat)
	wdt, okWDT := spbNodeByURI(g, spbWebDocType)
	if !okPF || !okWDT {
		return [][]any{}, nil
	}
	var rows []spbDated
	for w := range works.Iter() {
		if !spbHasRel(g, w, "tag") || !spbHasRel(g, w, "category") ||
			!spbHasRel(g, w, "thumbnail") || !spbHasRel(g, w, "audience") {
			continue
		}
		hasPF := false
		for t := range g.Neighbors(w, chickpeas.Outgoing, "primaryFormat") {
			if t == pf {
				hasPF = true
				break
			}
		}
		if !hasPF {
			continue
		}
		hasWDT := false
		for pc := range g.Neighbors(w, chickpeas.Outgoing, "primaryContentOf") {
			for t := range g.Neighbors(pc, chickpeas.Outgoing, "webDocumentType") {
				if t == wdt {
					hasWDT = true
					break
				}
			}
			if hasWDT {
				break
			}
		}
		if !hasWDT {
			continue
		}
		if dm, ok := g.Prop(w, "dateModified").Str(); ok {
			rows = append(rows, spbDated{w, dm})
		}
	}
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbA15 (advanced q15): title full-text works carrying a category rel
// and an about-/mentions-target pair sharing the forward-chained Thing
// entity type, sorted by id.
func spbA15(g *chickpeas.Snapshot) ([][]any, error) {
	var out []chickpeas.NodeID
	for w := range g.FullTextSearch("CreativeWork", "title", spbWord2).Iter() {
		if !spbHasRel(g, w, "category") {
			continue
		}
		aboutThing := false
		for a := range g.Neighbors(w, chickpeas.Outgoing, "about") {
			if g.HasLabel(a, spbEntityLabel) {
				aboutThing = true
				break
			}
		}
		if !aboutThing {
			continue
		}
		mentionsThing := false
		for m := range g.Neighbors(w, chickpeas.Outgoing, "mentions") {
			if g.HasLabel(m, spbEntityLabel) {
				mentionsThing = true
				break
			}
		}
		if mentionsThing {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return spbURIRows(g, out), nil
}

// spbA16 (advanced q16): distinct (work, tag) pairs of title full-text
// works with a category rel and a title, ordered by tag then work.
func spbA16(g *chickpeas.Snapshot) ([][]any, error) {
	type key struct{ tag, work string }
	seen := map[key]bool{}
	for w := range g.FullTextSearch("CreativeWork", "title", spbWord2).Iter() {
		if _, ok := g.Prop(w, "title").Str(); !ok {
			continue
		}
		if !spbHasRel(g, w, "category") {
			continue
		}
		workURI, ok := g.Prop(w, "uri").Str()
		if !ok {
			continue
		}
		for t := range g.Neighbors(w, chickpeas.Outgoing, "tag") {
			if tagURI, ok := g.Prop(t, "uri").Str(); ok {
				seen[key{tagURI, workURI}] = true
			}
		}
	}
	keys := make([]key, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sortByLess(keys, func(a, b key) bool {
		if a.tag != b.tag {
			return a.tag < b.tag
		}
		return a.work < b.work
	})
	rows := make([][]any, len(keys))
	for i, k := range keys {
		rows[i] = []any{k.work, k.tag}
	}
	return rows, nil
}

// spbA17 (advanced q17, geo): works mentioning a Feature inside the
// square box of half-extent deviation degrees around the reference
// point, each carrying a dateModified; deduped, sorted by id.
func spbA17(g *chickpeas.Snapshot) ([][]any, error) {
	cworks, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	features := g.GeoWithinBBox("Feature", "lat", "long",
		spbLat-spbDeviation, spbLon-spbDeviation, spbLat+spbDeviation, spbLon+spbDeviation)
	seen := map[chickpeas.NodeID]bool{}
	var out []chickpeas.NodeID
	for f := range features.Iter() {
		for w := range g.Neighbors(f, chickpeas.Incoming, "mentions") {
			if !cworks.Contains(w) || seen[w] {
				continue
			}
			seen[w] = true
			if _, ok := g.Prop(w, "dateModified").Str(); ok {
				out = append(out, w)
			}
		}
	}
	slices.Sort(out)
	return spbURIRows(g, out), nil
}

// spbA18 (advanced q18): most-recently-modified works of the type in
// the inclusive window carrying title, liveCoverage, and category +
// audience rels.
func spbA18(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel(spbCWType)
	if !ok {
		return [][]any{}, nil
	}
	var rows []spbDated
	for w := range works.Iter() {
		modified, ok := g.Prop(w, "dateModified").Str()
		if !ok || modified < spbDateFrom || modified > spbDateTo {
			continue
		}
		if _, ok := g.Prop(w, "title").Str(); !ok {
			continue
		}
		if _, ok := g.Prop(w, "liveCoverage").Value(); !ok {
			continue
		}
		if !spbHasRel(g, w, "category") || !spbHasRel(g, w, "audience") {
			continue
		}
		rows = append(rows, spbDated{w, modified})
	}
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbA19 (advanced q19): topics tagged by in-window works of the type /
// audience, ranked by newest tagging-work modification then count; each
// row renders the topic's label (uri fallback), count, and that newest
// date. Tag expands as about|mentions|tag deduped per work.
func spbA19(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]any{}, nil
	}
	startMs, endMs := spbParseMs(spbDateFrom), spbParseMs(spbDateTo)
	type agg struct {
		count int64
		ms    int64
		date  string
	}
	acc := map[chickpeas.NodeID]*agg{}
	for cw := range works.Iter() {
		if !g.HasLabel(cw, spbCWType) {
			continue
		}
		if !spbHasNeighborWithURI(g, cw, "audience", spbAudience) {
			continue
		}
		dt, ok := g.Prop(cw, "dateModified").Str()
		if !ok {
			continue
		}
		dtMs := spbParseMs(dt)
		if dtMs < startMs || dtMs > endMs {
			continue
		}
		topics := map[chickpeas.NodeID]bool{}
		for _, pred := range []string{"about", "mentions", "tag"} {
			for t := range g.Neighbors(cw, chickpeas.Outgoing, pred) {
				topics[t] = true
			}
		}
		for t := range topics {
			e := acc[t]
			if e == nil {
				e = &agg{ms: -1 << 62}
				acc[t] = e
			}
			e.count++
			if dtMs > e.ms {
				e.ms = dtMs
				e.date = dt
			}
		}
	}
	type row struct {
		id chickpeas.NodeID
		a  *agg
	}
	rows := make([]row, 0, len(acc))
	for t, a := range acc {
		rows = append(rows, row{t, a})
	}
	sortByLess(rows, func(a, b row) bool {
		if a.a.ms != b.a.ms {
			return a.a.ms > b.a.ms
		}
		if a.a.count != b.a.count {
			return a.a.count > b.a.count
		}
		return a.id < b.id
	})
	out := make([][]any, len(rows))
	for i, r := range rows {
		name, ok := g.Prop(r.id, "label").Str()
		if !ok {
			name = spbURIOf(g, r.id)
		}
		out[i] = []any{name, r.a.count, r.a.date}
	}
	return out, nil
}
