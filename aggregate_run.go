// Aggregation execution: the parallel grouped fold (Run) and the chained
// n-hop walk count (the Hop path).

package chickpeas

import (
	"fmt"
	"sync/atomic"

	"github.com/freeeve/gochickpeas/nodeset"
)

// groupKey is the composite group key: up to four inline dimensions (the
// common case, no per-row allocation), the rest spilled to a string of the
// raw i64 bytes. Comparable, so it keys the group map directly.
type groupKey struct {
	n    uint8
	dims [4]int64
	rest string
}

type keyBuilder struct {
	n    int
	dims [4]int64
	rest []int64
}

func (k *keyBuilder) push(v int64) {
	if k.n < 4 {
		k.dims[k.n] = v
	} else {
		k.rest = append(k.rest, v)
	}
	k.n++
}

func (k *keyBuilder) key() groupKey {
	out := groupKey{n: uint8(min(k.n, 255)), dims: k.dims}
	if len(k.rest) > 0 {
		buf := make([]byte, 0, len(k.rest)*8)
		for _, v := range k.rest {
			for shift := 0; shift < 64; shift += 8 {
				buf = append(buf, byte(uint64(v)>>shift))
			}
		}
		out.rest = string(buf)
	}
	return out
}

func (k groupKey) toSlice() []int64 {
	out := make([]int64, 0, int(k.n))
	for i := 0; i < int(k.n) && i < 4; i++ {
		out = append(out, k.dims[i])
	}
	for i := 0; i+8 <= len(k.rest); i += 8 {
		var v uint64
		for b := 7; b >= 0; b-- {
			v = v<<8 | uint64(k.rest[i+b])
		}
		out = append(out, int64(v))
	}
	return out
}

type resolvedGroup struct {
	kind   groupDimKind
	col    I64Col
	bounds []int64
	unit   TemporalUnit
	label  *nodeset.Set
}

type resolvedFilter struct {
	col   I64Col
	op    AggOp
	value int64
}

// colI64 resolves key to an i64 column reader (dense or sparse); errors
// when the column is absent or not i64 dtype.
func (g *Snapshot) colI64(key string) (I64Col, error) {
	col, ok := g.Col(key)
	if !ok {
		return I64Col{}, fmt.Errorf("%w: column %q not found", ErrSchema, key)
	}
	if col.Dtype() != DtypeI64 {
		return I64Col{}, fmt.Errorf("%w: column %q is not an i64 column", ErrSchema, key)
	}
	return col.I64(), nil
}

func (a *Aggregation) resolveFilters(filters []aggFilter) ([]resolvedFilter, error) {
	out := make([]resolvedFilter, 0, len(filters))
	for _, f := range filters {
		col, err := a.g.colI64(f.column)
		if err != nil {
			return nil, err
		}
		out = append(out, resolvedFilter{col: col, op: f.op, value: f.value})
	}
	return out, nil
}

type resolvedProjFilter struct {
	projection []NodeID
	column     Column
	allowed    map[Value]struct{}
}

func (a *Aggregation) resolveProjFilters() ([]resolvedProjFilter, error) {
	out := make([]resolvedProjFilter, 0, len(a.projFilters))
	for _, pf := range a.projFilters {
		keyID, ok := a.g.PropertyKey(pf.column)
		if !ok {
			return nil, fmt.Errorf("%w: column %q not found", ErrSchema, pf.column)
		}
		column, ok := a.g.columns[keyID]
		if !ok {
			return nil, fmt.Errorf("%w: column %q not found", ErrSchema, pf.column)
		}
		out = append(out, resolvedProjFilter{projection: pf.projection, column: column, allowed: pf.allowed})
	}
	return out, nil
}

func passes(node NodeID, filters []resolvedFilter) bool {
	for _, f := range filters {
		x, ok := f.col.Get(node)
		if !ok || !f.op.Test(x, f.value) {
			return false
		}
	}
	return true
}

// Run executes the reduction in parallel and collects the groups.
func (a *Aggregation) Run() (*AggResult, error) {
	if len(a.hops) > 0 {
		if err := a.checkHopsSupported(); err != nil {
			return nil, err
		}
		return a.runHops()
	}
	g := a.g
	filters, err := a.resolveFilters(a.filters)
	if err != nil {
		return nil, err
	}
	having, err := a.resolveFilters(a.having)
	if err != nil {
		return nil, err
	}
	projFilters, err := a.resolveProjFilters()
	if err != nil {
		return nil, err
	}
	gspecs := make([]resolvedGroup, 0, len(a.group))
	for _, d := range a.group {
		switch d.kind {
		case dimLabel:
			set, ok := g.NodesWithLabel(d.column)
			if !ok {
				return nil, fmt.Errorf("%w: label %q not found", ErrSchema, d.column)
			}
			gspecs = append(gspecs, resolvedGroup{kind: dimLabel, label: set})
		default:
			col, err := g.colI64(d.column)
			if err != nil {
				return nil, err
			}
			gspecs = append(gspecs, resolvedGroup{kind: d.kind, col: col, bounds: d.bounds, unit: d.unit})
		}
	}
	present := make([]Column, 0, len(a.requirePresent))
	for _, name := range a.requirePresent {
		keyID, ok := g.PropertyKey(name)
		if !ok {
			return nil, fmt.Errorf("%w: column %q not found", ErrSchema, name)
		}
		column, ok := g.columns[keyID]
		if !ok {
			return nil, fmt.Errorf("%w: column %q not found", ErrSchema, name)
		}
		present = append(present, column)
	}
	var sumCol I64Col
	if a.hasSum {
		if sumCol, err = g.colI64(a.sumCol); err != nil {
			return nil, err
		}
	}
	sets := make([]*nodeset.Set, 0, len(a.labels))
	for _, l := range a.labels {
		set, ok := g.NodesWithLabel(l)
		if !ok {
			return nil, fmt.Errorf("%w: label %q not found", ErrSchema, l)
		}
		sets = append(sets, set)
	}
	var throughMatch RelMatch
	if a.hasThrough {
		throughMatch = g.Match(a.through)
	}

	type agg struct {
		count uint64
		sum   int64
	}
	type partial struct {
		groups map[groupKey]*agg
		total  uint64
	}
	var bailed atomic.Bool
	groups := map[groupKey]*agg{}
	var total uint64

	for labelIdx, set := range sets {
		part := nodeset.ParFold(set,
			func() *partial { return &partial{groups: map[groupKey]*agg{}} },
			func(acc *partial, node NodeID) *partial {
				// A filter value that is absent excludes the node -- a Null
				// never satisfies a comparison.
				if !passes(node, filters) {
					return acc
				}
				for _, c := range present {
					if _, ok := c.Get(node); !ok {
						return acc
					}
				}
				for _, pf := range projFilters {
					if int(node) >= len(pf.projection) {
						return acc
					}
					v, ok := pf.column.Get(pf.projection[node])
					if !ok {
						return acc
					}
					if _, allowed := pf.allowed[v]; !allowed {
						return acc
					}
				}
				acc.total++
				if !passes(node, having) {
					return acc
				}
				var kb keyBuilder
				if a.byLabel {
					kb.push(int64(labelIdx))
				}
				for _, spec := range gspecs {
					switch spec.kind {
					case dimCol:
						v, ok := spec.col.Get(node)
						if !ok {
							// An absent plain group value can't be keyed as
							// i64 -- surface it as an error after the fold.
							bailed.Store(true)
							return acc
						}
						kb.push(v)
					case dimBin:
						// A bin over an absent value is the ELSE bucket.
						bucket := int64(len(spec.bounds))
						if v, ok := spec.col.Get(node); ok {
							bucket = 0
							for _, bound := range spec.bounds {
								if v >= bound {
									bucket++
								}
							}
						}
						kb.push(bucket)
					case dimComponent:
						v, ok := spec.col.Get(node)
						if !ok {
							bailed.Store(true)
							return acc
						}
						kb.push(spec.unit.of(v))
					case dimLabel:
						if spec.label.Contains(node) {
							kb.push(1)
						} else {
							kb.push(0)
						}
					}
				}
				bump := func(key groupKey) {
					e, ok := acc.groups[key]
					if !ok {
						e = &agg{}
						acc.groups[key] = e
					}
					e.count++
					if a.hasSum {
						if v, ok := sumCol.Get(node); ok {
							e.sum += v
						}
					}
				}
				if a.hasThrough {
					for nb := range g.NeighborsMatch(node, a.throughDir, throughMatch) {
						if a.neighborFilter != nil {
							if _, ok := a.neighborFilter[nb]; !ok {
								continue
							}
						}
						nkb := kb
						nkb.rest = append([]int64(nil), kb.rest...)
						nkb.push(int64(nb))
						bump(nkb.key())
					}
				} else {
					bump(kb.key())
				}
				return acc
			},
			func(x, y *partial) *partial {
				for k, e := range y.groups {
					if cur, ok := x.groups[k]; ok {
						cur.count += e.count
						cur.sum += e.sum
					} else {
						x.groups[k] = e
					}
				}
				x.total += y.total
				return x
			})
		for k, e := range part.groups {
			if cur, ok := groups[k]; ok {
				cur.count += e.count
				cur.sum += e.sum
			} else {
				groups[k] = e
			}
		}
		total += part.total
	}
	if bailed.Load() {
		return nil, fmt.Errorf("%w: group column has absent values; use a generic aggregation path", ErrSchema)
	}

	rows := make([]AggRow, 0, len(groups))
	for k, e := range groups {
		rows = append(rows, AggRow{Key: k.toSlice(), Count: e.count, Sum: e.sum})
	}
	fields := make([]string, 0, len(a.group)+2)
	if a.byLabel {
		fields = append(fields, "label")
	}
	for _, d := range a.group {
		switch d.kind {
		case dimCol:
			fields = append(fields, d.column)
		case dimBin:
			fields = append(fields, d.column+"_bin")
		case dimComponent:
			fields = append(fields, d.column+"_"+d.unit.suffix())
		case dimLabel:
			fields = append(fields, d.column)
		}
	}
	if a.hasThrough {
		fields = append(fields, "neighbor")
	}
	return &AggResult{Total: total, Rows: rows, Fields: fields}, nil
}

// checkHopsSupported rejects option combinations the walk path would
// silently ignore.
func (a *Aggregation) checkHopsSupported() error {
	for _, bad := range []struct {
		set  bool
		name string
	}{
		{len(a.having) > 0, "Having"},
		{a.byLabel, "ByLabel"},
		{len(a.group) > 0, "By/Bin/TemporalComponent/ByLabelMembership"},
		{a.hasSum, "Sum"},
		{a.hasThrough, "Through"},
		{a.neighborFilter != nil, "OnlyNeighbors"},
		{len(a.requirePresent) > 0, "RequirePresent"},
	} {
		if bad.set {
			return fmt.Errorf("%w: chained Hop walk counts do not support %s (it would be silently ignored)", ErrSchema, bad.name)
		}
	}
	return nil
}

// runHops is the chained n-hop walk count: seed per-node walk multiplicity
// from the filtered sources, expand hop by hop over the CSR accumulating
// next[neighbor] += mult[node] (two dense buffers ping-pong, frontier
// tracked so clears are O(active)), then emit one row per endpoint.
func (a *Aggregation) runHops() (*AggResult, error) {
	g := a.g
	filters, err := a.resolveFilters(a.filters)
	if err != nil {
		return nil, err
	}
	projFilters, err := a.resolveProjFilters()
	if err != nil {
		return nil, err
	}
	sets := make([]*nodeset.Set, 0, len(a.labels))
	for _, l := range a.labels {
		set, ok := g.NodesWithLabel(l)
		if !ok {
			return nil, fmt.Errorf("%w: label %q not found", ErrSchema, l)
		}
		sets = append(sets, set)
	}
	hops := make([]struct {
		m   RelMatch
		dir Direction
	}, len(a.hops))
	for i, h := range a.hops {
		// A missing type matches nothing, so the walk dies there.
		hops[i].m = g.Match(h.RelType)
		hops[i].dir = h.Dir
	}

	// Walk-multiplicity buffers size by the CSR id space, not NodeCount:
	// they index by raw node id (sparse-id safety).
	n := int(g.CSRIDSpace())
	mult := make([]uint64, n)
	next := make([]uint64, n)
	var frontier []NodeID
	var total uint64
	for _, set := range sets {
		for node := range set.Iter() {
			if !passes(node, filters) {
				continue
			}
			skip := false
			for _, pf := range projFilters {
				if int(node) >= len(pf.projection) {
					skip = true
					break
				}
				v, ok := pf.column.Get(pf.projection[node])
				if !ok {
					skip = true
					break
				}
				if _, allowed := pf.allowed[v]; !allowed {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if mult[node] == 0 {
				frontier = append(frontier, node)
			}
			mult[node]++
			total++
		}
	}

	for _, hop := range hops {
		var nextFrontier []NodeID
		for _, node := range frontier {
			m := mult[node]
			for nb := range g.NeighborsMatch(node, hop.dir, hop.m) {
				if next[nb] == 0 {
					nextFrontier = append(nextFrontier, nb)
				}
				next[nb] += m
			}
		}
		// Clear the spent multiplicities (O(active)), then swap so next is
		// the all-zero scratch for the following hop.
		for _, node := range frontier {
			mult[node] = 0
		}
		mult, next = next, mult
		frontier = nextFrontier
	}

	rows := make([]AggRow, 0, len(frontier))
	for _, endpoint := range frontier {
		rows = append(rows, AggRow{Key: []int64{int64(endpoint)}, Count: mult[endpoint]})
	}
	return &AggResult{Total: total, Rows: rows, Fields: []string{"endpoint"}}, nil
}
