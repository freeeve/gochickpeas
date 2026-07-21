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
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// nodeCounter is the flat per-node tally the counting kernels share: a
// packed-key index into parallel node/count slabs. The Go map it
// replaces paid its bucket-growth ladder per run on every kernel tallying
// tens of thousands of entities.
type nodeCounter struct {
	idx    flatset.U64Map
	nodes  []chickpeas.NodeID
	counts []int64
}

func (c *nodeCounter) bump(n chickpeas.NodeID) { c.add(n, 1) }

func (c *nodeCounter) add(n chickpeas.NodeID, d int64) {
	i := c.idx.GetOrCreate(uint64(n), func() int {
		c.nodes = append(c.nodes, n)
		c.counts = append(c.counts, 0)
		return len(c.counts) - 1
	})
	c.counts[i] += d
}

// get returns n's tally, zero when never counted.
func (c *nodeCounter) get(n chickpeas.NodeID) int64 {
	if i, ok := c.idx.Get(uint64(n)); ok {
		return c.counts[i]
	}
	return 0
}

// spbCountRowsFlat is spbCountRows over the flat counter.
func spbCountRowsFlat(g *chickpeas.Snapshot, c *nodeCounter) [][]value.Value {
	type kv struct {
		uri string
		n   int64
	}
	tmp := make([]kv, 0, len(c.nodes))
	for i, n := range c.nodes {
		tmp = append(tmp, kv{spbURIOf(g, n), c.counts[i]})
	}
	sortByLess(tmp, func(a, b kv) bool {
		if a.n != b.n {
			return a.n > b.n
		}
		return a.uri < b.uri
	})
	cells := make([]value.Value, len(tmp)*2)
	rows := make([][]value.Value, len(tmp))
	for i, r := range tmp {
		cells[i*2] = value.Str(r.uri)
		cells[i*2+1] = value.Int(r.n)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows
}

func init() {
	registerNativeV("SPB", "a1", simpleKernelV(spbA1))
	registerNativeV("SPB", "a2", spbA2)
	registerNativeV("SPB", "a3", simpleKernelV(spbA3))
	registerNativeV("SPB", "a4", simpleKernelV(spbA4))
	registerNativeV("SPB", "a5", simpleKernelV(spbA5))
	registerNativeV("SPB", "a6", simpleKernelV(spbA6))
	registerNativeV("SPB", "a7", simpleKernelV(spbA7))
	registerNativeV("SPB", "a8", simpleKernelV(spbA8))
	registerNativeV("SPB", "a9", simpleKernelV(spbA9))
	registerNativeV("SPB", "a10", simpleKernelV(spbA10))
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
// spbCountRows renders a node->count map as (uri, count) rows, count
// descending then uri ascending. Self-contained value.Value result: sorts a
// typed (uri, count) slice and carves fixed-cap row views out of one flat
// backing.
func spbCountRows(g *chickpeas.Snapshot, counts map[chickpeas.NodeID]int64) [][]value.Value {
	type kv struct {
		uri string
		n   int64
	}
	tmp := make([]kv, 0, len(counts))
	for n, c := range counts {
		tmp = append(tmp, kv{spbURIOf(g, n), c})
	}
	sortByLess(tmp, func(a, b kv) bool {
		if a.n != b.n {
			return a.n > b.n
		}
		return a.uri < b.uri
	})
	cells := make([]value.Value, len(tmp)*2)
	rows := make([][]value.Value, len(tmp))
	for i, r := range tmp {
		cells[i*2] = value.Str(r.uri)
		cells[i*2+1] = value.Int(r.n)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows
}

// spbA1 (advanced q1): creative works with an `about` rel to the topic
// and a dateModified, newest first.
func spbA1(g *chickpeas.Snapshot) ([][]value.Value, error) {
	topic, ok := spbNodeByURI(g, spbTopic)
	if !ok {
		return [][]value.Value{}, nil
	}
	cworks, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
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
func spbA2(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cwURI := spbQ2CW(g)
	return func() ([][]value.Value, error) {
		work, ok := spbNodeByURI(g, cwURI)
		if !ok || !g.HasLabel(work, "CreativeWork") {
			return [][]value.Value{}, nil
		}
		if _, hasTitle := g.Prop(work, "title").Str(); !hasTitle {
			return [][]value.Value{}, nil
		}
		var rows [][]value.Value
		for _, label := range spbSubtypes {
			if g.HasLabel(work, label) {
				rows = append(rows, []value.Value{value.Str(label)})
			}
		}
		return rows, nil
	}, nil
}

// spbA3 (advanced q3): in-window works counted by the minute-of-hour of
// dateModified (the SPARQL MINUTES), minute emitted as a bare decimal
// string.
func spbA3(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
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
	cells := make([]value.Value, len(counts)*2)
	rows := make([][]value.Value, 0, len(counts))
	i := 0
	for m, c := range counts {
		cells[i*2] = value.Str(strconvItoa(m))
		cells[i*2+1] = value.Int(c)
		rows = append(rows, cells[i*2:i*2+2:i*2+2])
		i++
	}
	spbSortKVV(rows)
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
func spbA4(g *chickpeas.Snapshot) ([][]value.Value, error) {
	cells := make([]value.Value, len(spbSubtypes)*2)
	rows := make([][]value.Value, 0, len(spbSubtypes))
	i := 0
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
			cells[i*2] = value.Str(label)
			cells[i*2+1] = value.Int(n)
			rows = append(rows, cells[i*2:i*2+2:i*2+2])
			i++
		}
	}
	spbSortKVV(rows)
	return rows, nil
}

// spbA5 (advanced q5): about-targets of the entity type ranked by how
// many works category-linked to either pinned category are about them
// (per about rel, duplicates included).
func spbA5(g *chickpeas.Snapshot) ([][]value.Value, error) {
	entities, ok := g.NodesWithLabel(spbEntityLabel)
	if !ok {
		return [][]value.Value{}, nil
	}
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
	}
	var counts nodeCounter
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
				counts.bump(about)
			}
		}
	}
	return spbCountRowsFlat(g, &counts), nil
}

// spbA6 (advanced q6): about-entity types (leaf classes plus the
// forward-chained Thing) ranked by covered (work, about) pairs, over
// works with the live flag and audience.
func spbA6(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
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
	cells := make([]value.Value, len(counts)*2)
	rows := make([][]value.Value, 0, len(counts))
	i := 0
	for ty, n := range counts {
		cells[i*2] = value.Str(ty)
		cells[i*2+1] = value.Int(n)
		rows = append(rows, cells[i*2:i*2+2:i*2+2])
		i++
	}
	spbSortKVV(rows)
	return rows, nil
}

// spbA7 (advanced q7): mention targets ranked by mentions from works
// whose primaryContentOf out-degree exceeds the threshold (1).
func spbA7(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
	}
	var counts nodeCounter
	for w := range works.Iter() {
		if spbOutCount(g, w, "primaryContentOf") <= 1 {
			continue
		}
		for m := range g.Neighbors(w, chickpeas.Outgoing, "mentions") {
			counts.bump(m)
		}
	}
	return spbCountRowsFlat(g, &counts), nil
}

// spbA8 (advanced q8): topics ranked by tag rels (the loader's
// materialized about/mentions super-property, counted per rel) from
// works of the type/audience inside the dateModified window.
func spbA8(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel(spbCWType)
	if !ok {
		return [][]value.Value{}, nil
	}
	var counts nodeCounter
	for w := range works.Iter() {
		dt, ok := g.Prop(w, "dateModified").Str()
		if !ok || !(dt > spbDateFrom && dt < spbDateTo) {
			continue
		}
		if !spbHasNeighborWithURI(g, w, "audience", spbAudience) {
			continue
		}
		for t := range g.Neighbors(w, chickpeas.Outgoing, "tag") {
			counts.bump(t)
		}
	}
	return spbCountRowsFlat(g, &counts), nil
}

// spbA9 (advanced q9): the largest outgoing mentions count on any
// single creative work, as the one-row [max, n] aggregate the parity
// ref stores.
func spbA9(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{{value.Str("max"), value.Int(0)}}, nil
	}
	var maxN int64
	for w := range works.Iter() {
		if n := spbOutCount(g, w, "mentions"); n > maxN {
			maxN = n
		}
	}
	return [][]value.Value{{value.Str("max"), value.Int(maxN)}}, nil
}

// spbA10 (advanced q10): every dateCreated-carrying work whose mentions
// count attains the global maximum, as [uri, count] ordered by uri.
func spbA10(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
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
		return [][]value.Value{}, nil
	}
	type row struct {
		uri string
		n   int64
	}
	var tmp []row
	for _, c := range counts {
		if c.n != maxN {
			continue
		}
		if _, ok := g.Prop(c.id, "dateCreated").Str(); !ok {
			continue
		}
		tmp = append(tmp, row{spbURIOf(g, c.id), c.n})
	}
	sortByLess(tmp, func(a, b row) bool { return a.uri < b.uri })
	cells := make([]value.Value, len(tmp)*2)
	rows := make([][]value.Value, len(tmp))
	for i, r := range tmp {
		cells[i*2] = value.Str(r.uri)
		cells[i*2+1] = value.Int(r.n)
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}
