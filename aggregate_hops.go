// Chained n-hop walk-count aggregation: the multi-hop path of Aggregation,
// which seeds per-node walk multiplicity from the filtered sources and
// expands hop by hop over the CSR (two dense buffers ping-pong, frontier
// tracked so clears are O(active)), emitting one row per reached endpoint.
// Split from aggregate_run.go, which holds the single-scan Run.
package chickpeas

import (
	"fmt"

	"github.com/freeeve/gochickpeas/nodeset"
)

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
