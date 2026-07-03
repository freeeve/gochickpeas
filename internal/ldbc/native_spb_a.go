// Native SPB kernels, basics q1-q9 -- ports of rustychickpeas-ldbc
// src/spb/{q1,q2,q3,q4,q5,q7,q9}.rs onto the SPB property graph
// (spbload.go / cmd/spbexport). Substitution parameters are the fixed,
// data-derived set their parity runner pins (src/spb/parity.rs), also
// recorded in python/refs/spb/spb.parity.rust.json's params block; q2's
// concrete creative work derives from q1's top row and is resolved in
// the untimed prepare, exactly as the Rust harness computes it outside
// its timer. cwork:tag has no materialized rel in the extract, so
// "tagged" is the about|mentions union throughout.

package ldbc

import (
	"math"
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
)

// The fixed SPB parameter set (src/spb/parity.rs).
const (
	spbWord          = "football"
	spbWord2         = "policy" // rarer title word for the star queries (a15/a16)
	spbTopic         = "http://dbpedia.org/resource/Action_of_25_February_1781"
	spbEntB          = "http://dbpedia.org/resource/International_Telecoms_Week"
	spbCategory      = "http://www.bbc.co.uk/category/Event"
	spbCatCompany    = "http://www.bbc.co.uk/category/Company"
	spbEntityLabel   = "Thing" // coreconcepts:Thing via subClassOf, as a label
	spbPrimaryFormat = "http://www.bbc.co.uk/ontologies/creativework/TextualFormat"
	spbWebDocType    = "http://www.bbc.co.uk/ontologies/bbc/HighWeb"
	spbAudience      = "http://www.bbc.co.uk/ontologies/creativework/InternationalAudience"
	spbCWType        = "BlogPost"
	spbDateFrom      = "2011-03-01T00:00:00.000+00:00"
	spbDateTo        = "2011-06-01T00:00:00.000+00:00"
	spbLat           = 51.5074
	spbLon           = -0.1278
	spbDeviation     = 0.5
	spbAll           = math.MaxInt // their parity runner disables LIMITs
)

// spbTagPreds are the sub-properties composing cwork:tag; the loader
// also materializes the entailed `tag` rel, so traversing these two
// covers the tag semantics without double work.
var spbTagPreds = []string{"about", "mentions"}

func init() {
	registerNative("SPB", "q1", simpleKernel(spbQ1))
	registerNative("SPB", "q2", spbQ2)
	registerNative("SPB", "q3", simpleKernel(spbQ3))
	registerNative("SPB", "q4", simpleKernel(spbQ4))
	registerNative("SPB", "q5", simpleKernel(spbQ5))
	registerNative("SPB", "q7", simpleKernel(spbQ7))
	registerNative("SPB", "q9", spbQ9)
}

// spbNodeByURI resolves a node by its uri property (label-free lookup).
func spbNodeByURI(g *chickpeas.Snapshot, uri string) (chickpeas.NodeID, bool) {
	return g.NodeWithProperty("uri", uri)
}

// spbURIOf reads a node's uri, "?" when absent (the Rust emitters'
// unwrap_or).
func spbURIOf(g *chickpeas.Snapshot, n chickpeas.NodeID) string {
	if u, ok := g.Prop(n, "uri").Str(); ok {
		return u
	}
	return "?"
}

// spbURIRows renders ranked node ids as one-cell uri rows (the parity
// JSON's uris kind).
func spbURIRows(g *chickpeas.Snapshot, ids []chickpeas.NodeID) [][]any {
	rows := make([][]any, len(ids))
	for i, n := range ids {
		rows[i] = []any{spbURIOf(g, n)}
	}
	return rows
}

// spbParseMs parses an ISO-8601 dateTime string to epoch-ms, 0 when the
// date part is malformed (the Rust props::parse_ms).
func spbParseMs(s string) int64 {
	if len(s) < 10 {
		return 0
	}
	y, okY := spbAtoi(s[0:4])
	m, okM := spbAtoi(s[5:7])
	d, okD := spbAtoi(s[8:10])
	if !okY || !okM || !okD {
		return 0
	}
	day := dayFromCivil(y, m, d)
	var h, mi, se, ms int64
	if len(s) >= 13 {
		h, _ = spbAtoi(s[11:13])
	}
	if len(s) >= 16 {
		mi, _ = spbAtoi(s[14:16])
	}
	if len(s) >= 19 {
		se, _ = spbAtoi(s[17:19])
	}
	if len(s) >= 23 && s[19] == '.' {
		ms, _ = spbAtoi(s[20:23])
	}
	return day*86_400_000 + h*3_600_000 + mi*60_000 + se*1_000 + ms
}

// spbAtoi parses a small unsigned decimal field.
func spbAtoi(s string) (int64, bool) {
	var v int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	return v, len(s) > 0
}

// spbHasNeighborWithURI reports an outgoing rel whose target carries the
// uri (the Rust has_neighbor_with_property facet check).
func spbHasNeighborWithURI(g *chickpeas.Snapshot, n chickpeas.NodeID, rel, uri string) bool {
	for nb := range g.Neighbors(n, chickpeas.Outgoing, rel) {
		if u, ok := g.Prop(nb, "uri").Str(); ok && u == uri {
			return true
		}
	}
	return false
}

// spbHasRel reports whether any outgoing rel of the type exists (the
// SPARQL "bound" check).
func spbHasRel(g *chickpeas.Snapshot, n chickpeas.NodeID, rel string) bool {
	_, ok := g.FirstNeighbor(n, chickpeas.Outgoing, rel)
	return ok
}

// spbTaggingWorks collects the distinct label-set members reaching the
// topic through incoming about/mentions (cwork:tag), each carrying the
// named required date property; absent-date works are excluded.
func spbTaggingWorks(g *chickpeas.Snapshot, topicURI, label, dateKey string) []spbDated {
	topic, ok := spbNodeByURI(g, topicURI)
	if !ok {
		return nil
	}
	set, ok := g.NodesWithLabel(label)
	if !ok {
		return nil
	}
	seen := map[chickpeas.NodeID]bool{}
	var rows []spbDated
	for _, pred := range spbTagPreds {
		for w := range g.Neighbors(topic, chickpeas.Incoming, pred) {
			if !set.Contains(w) || seen[w] {
				continue
			}
			seen[w] = true
			if dt, ok := g.Prop(w, dateKey).Str(); ok {
				rows = append(rows, spbDated{w, dt})
			}
		}
	}
	return rows
}

// spbDated is one (work, ISO date string) ranking row.
type spbDated struct {
	id chickpeas.NodeID
	dt string
}

// spbRankDated orders date descending (ISO-8601 sorts lexicographically)
// with node id ascending on ties, truncated to limit.
func spbRankDated(rows []spbDated, limit int) []chickpeas.NodeID {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].dt != rows[j].dt {
			return rows[i].dt > rows[j].dt
		}
		return rows[i].id < rows[j].id
	})
	if limit >= 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	ids := make([]chickpeas.NodeID, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids
}

// spbQ1Rank is q1's ranked work list, shared by q2/q9/a2's derived
// creative-work parameter.
func spbQ1Rank(g *chickpeas.Snapshot) []chickpeas.NodeID {
	return spbRankDated(spbTaggingWorks(g, spbTopic, "CreativeWork", "dateModified"), spbAll)
}

// spbQ2CW is the derived q2 parameter: the uri of q1's newest work (""
// when q1 is empty), computed in untimed prepares like the Rust
// harness's q2_cw.
func spbQ2CW(g *chickpeas.Snapshot) string {
	if rank := spbQ1Rank(g); len(rank) > 0 {
		return spbURIOf(g, rank[0])
	}
	return ""
}

// spbQ1 (basic q1): creative works about|mentions the topic, ranked by
// dateModified descending.
func spbQ1(g *chickpeas.Snapshot) ([][]any, error) {
	return spbURIRows(g, spbQ1Rank(g)), nil
}

// spbQ2 (basic q2): describe one concrete creative work -- the
// CreativeWork with the derived uri, only if it carries the required
// title. The parameter derivation is untimed prepare; the timed run is
// the lookup itself.
func spbQ2(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cwURI := spbQ2CW(g)
	return func() ([][]any, error) {
		set, ok := g.NodesWithProperty("CreativeWork", "uri", cwURI)
		if !ok {
			return [][]any{}, nil
		}
		for n := range set.Iter() {
			if _, hasTitle := g.Prop(n, "title").Str(); hasTitle {
				return [][]any{{spbURIOf(g, n)}}, nil
			}
			break // first (lowest-id) member only, like the Rust iter().next()
		}
		return [][]any{}, nil
	}, nil
}

// spbQ3 (basic q3): works tagging the topic with a dateCreated, newest
// first.
func spbQ3(g *chickpeas.Snapshot) ([][]any, error) {
	rows := spbTaggingWorks(g, spbTopic, "CreativeWork", "dateCreated")
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbQ4 (basic q4): blog posts tagging the topic with a dateCreated,
// newest first.
func spbQ4(g *chickpeas.Snapshot) ([][]any, error) {
	rows := spbTaggingWorks(g, spbTopic, spbCWType, "dateCreated")
	return spbURIRows(g, spbRankDated(rows, spbAll)), nil
}

// spbQ5 (basic q5): per labelled topic, how many works of the type /
// audience tag it inside the exclusive dateModified window; count
// descending, label ascending.
func spbQ5(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel(spbCWType)
	if !ok {
		return [][]any{}, nil
	}
	startMs, endMs := spbParseMs(spbDateFrom), spbParseMs(spbDateTo)
	counts := map[chickpeas.NodeID]int64{}
	for cw := range works.Iter() {
		if !spbHasNeighborWithURI(g, cw, "audience", spbAudience) {
			continue
		}
		dt, ok := g.Prop(cw, "dateModified").Str()
		if !ok {
			continue
		}
		if ms := spbParseMs(dt); !(ms > startMs && ms < endMs) {
			continue
		}
		topics := map[chickpeas.NodeID]bool{}
		for _, pred := range spbTagPreds {
			for t := range g.Neighbors(cw, chickpeas.Outgoing, pred) {
				topics[t] = true
			}
		}
		for t := range topics {
			if _, hasLabel := g.Prop(t, "label").Str(); hasLabel {
				counts[t]++
			}
		}
	}
	rows := make([][]any, 0, len(counts))
	for t, n := range counts {
		label, _ := g.Prop(t, "label").Str()
		rows = append(rows, []any{label, n})
	}
	spbSortKV(rows)
	return rows, nil
}

// spbSortKV orders [key, count] rows count descending then key
// ascending (the aggregate queries' shared ORDER BY).
func spbSortKV(rows [][]any) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i][1].(int64), rows[j][1].(int64)
		if a != b {
			return a > b
		}
		return rows[i][0].(string) < rows[j][0].(string)
	})
}

// spbQ7 (basic q7): date-range retrieval with facets -- works of the
// type created inside [from, to] carrying title and liveCoverage, with
// category/audience rels pinned to the parameter uris.
func spbQ7(g *chickpeas.Snapshot) ([][]any, error) {
	works, ok := g.NodesWithLabel(spbCWType)
	if !ok {
		return [][]any{}, nil
	}
	var out []chickpeas.NodeID
	for w := range works.Iter() {
		created, ok := g.Prop(w, "dateCreated").Str()
		if !ok || created < spbDateFrom || created > spbDateTo {
			continue
		}
		if _, hasTitle := g.Prop(w, "title").Str(); !hasTitle {
			continue
		}
		if _, hasLive := g.Prop(w, "liveCoverage").Value(); !hasLive {
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

// spbQ9 (basic q9): works related to the derived creative work by
// shared tagged entities, scored 2*(about,about) + 1.5*(about,mentions)
// + 1*(mentions,about) + 0.5*(mentions,mentions); the emitted score is
// doubled to an integer, matching the stored ref (which sidesteps
// decimal-coefficient quirks on the SPARQL side).
func spbQ9(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cwURI := spbQ2CW(g)
	return func() ([][]any, error) {
		focal, ok := spbNodeByURI(g, cwURI)
		if !ok {
			return [][]any{}, nil
		}
		focalAbout := map[chickpeas.NodeID]bool{}
		for e := range g.Neighbors(focal, chickpeas.Outgoing, "about") {
			focalAbout[e] = true
		}
		focalMentions := map[chickpeas.NodeID]bool{}
		for e := range g.Neighbors(focal, chickpeas.Outgoing, "mentions") {
			focalMentions[e] = true
		}

		candidates := map[chickpeas.NodeID]bool{}
		collect := func(ents map[chickpeas.NodeID]bool) {
			for ent := range ents {
				for _, rel := range spbTagPreds {
					for w := range g.Neighbors(ent, chickpeas.Incoming, rel) {
						if w != focal && g.HasLabel(w, "CreativeWork") {
							candidates[w] = true
						}
					}
				}
			}
		}
		collect(focalAbout)
		collect(focalMentions)

		var rows [][]any
		for o := range candidates {
			if _, hasDT := g.Prop(o, "dateModified").Str(); !hasDT {
				continue
			}
			var a2a, a2m, m2a, m2m int64
			for e := range g.Neighbors(o, chickpeas.Outgoing, "about") {
				if focalAbout[e] {
					a2a++
				}
				if focalMentions[e] {
					m2a++
				}
			}
			for e := range g.Neighbors(o, chickpeas.Outgoing, "mentions") {
				if focalAbout[e] {
					a2m++
				}
				if focalMentions[e] {
					m2m++
				}
			}
			// score*2 stays integral: 4*a2a + 3*a2m + 2*m2a + m2m.
			score2 := 4*a2a + 3*a2m + 2*m2a + m2m
			if score2 <= 0 {
				continue
			}
			rows = append(rows, []any{spbURIOf(g, o), score2})
		}
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i][1].(int64), rows[j][1].(int64)
			if a != b {
				return a > b
			}
			return rows[i][0].(string) < rows[j][0].(string)
		})
		return rows, nil
	}, nil
}
