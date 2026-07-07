// First-claim value propagation (PropagateBFS): a depth-bounded BFS from
// seeded nodes where the first relationship, in BFS order, to reach a node
// claims it and sets the value the node carries onward. The claim order --
// FIFO over discovery, with each expansion's eligible rels ordered by a rel
// property and optionally truncated -- is part of the semantics, not a perf
// detail: which rels survive truncation decides which rel claims which
// neighbor. Each seed runs an independent traversal (its own visited set);
// across seeds a node accumulates the sum of its per-seed claimed values
// and its minimum depth. This is the stateful money-flow/taint-trace shape
// (e.g. LDBC FinBench's truncated fund tracing) that path enumeration plus
// aggregation cannot reproduce and that explodes on hubs if attempted.

package chickpeas

import "sort"

// PropagateSeed is one traversal start: the node enters at depth 1 carrying
// Value. The same node may be seeded more than once (one run each).
type PropagateSeed struct {
	Node  NodeID
	Value float64
}

// PropagateResult is one reached node with its accumulated value and
// minimum depth (seeds are depth 1).
type PropagateResult struct {
	Node  NodeID
	Value float64
	Depth uint32
}

// PropagateOpts parameterizes PropagateBFS. The zero value is not useful:
// set RelTypes (empty matches no rels), Direction, MaxDepth, and ValueProp.
type PropagateOpts struct {
	// RelTypes is the fan-out union of relationship types.
	RelTypes []string
	// Direction of expansion from each claimed node.
	Direction Direction
	// MaxDepth bounds the traversal: seeds sit at depth 1 and nodes expand
	// while their depth is below MaxDepth (values below 1 mean seeds only).
	MaxDepth uint32
	// ValueProp is the float rel property a claiming rel carries to the
	// node it claims (absent values read as 0).
	ValueProp string
	// Desc orders each expansion's eligible rels by ValueProp descending
	// instead of ascending. Ties keep adjacency order (stable).
	Desc bool
	// TruncLimit caps each expansion's ordered rels (0 = no cap).
	TruncLimit int
	// MinValue is an exclusive lower bound on a claiming rel's carried
	// value; the default 0 propagates only positive values. Pass -Inf to
	// disable.
	MinValue float64
	// FilterProp, when non-empty, names an integer rel property that must
	// be present and within [FilterMin, FilterMax] for a rel to be
	// eligible at all.
	FilterProp           string
	FilterMin, FilterMax int64
}

// propEntry is one node's accumulation across per-seed runs.
type propEntry struct {
	value float64
	depth uint32
}

// propHop is a queued claim: node claimed at depth carrying value.
type propHop struct {
	node  NodeID
	depth uint32
	value float64
}

// propRel is one eligible fan-out rel during an expansion.
type propRel struct {
	value float64
	nbr   NodeID
}

// PropagateBFS runs first-claim value propagation from seeds under opts.
// Results are sorted by node id ascending; a seed node itself is a result
// (depth 1, its seed value) even when it expands nowhere.
func (g *Snapshot) PropagateBFS(seeds []PropagateSeed, opts PropagateOpts) []PropagateResult {
	var valCol F64Col
	haveVal := false
	if opts.ValueProp != "" {
		if c, ok := g.RelCol(opts.ValueProp); ok && c.Dtype() == DtypeF64 {
			valCol = c.F64()
			haveVal = true
		}
	}
	var filtCol I64Col
	haveFilt := false
	if opts.FilterProp != "" {
		if c, ok := g.RelCol(opts.FilterProp); ok && c.Dtype() == DtypeI64 {
			filtCol = c.I64()
			haveFilt = true
		}
	}
	filtering := opts.FilterProp != ""

	acc := map[NodeID]*propEntry{}
	var queue []propHop
	var rels []propRel
	for _, seed := range seeds {
		visited := map[NodeID]bool{seed.Node: true}
		queue = append(queue[:0], propHop{seed.Node, 1, seed.Value})
		for qi := 0; qi < len(queue); qi++ {
			cur := queue[qi]
			if e := acc[cur.node]; e != nil {
				e.value += cur.value
				if cur.depth < e.depth {
					e.depth = cur.depth
				}
			} else {
				acc[cur.node] = &propEntry{cur.value, cur.depth}
			}
			if cur.depth >= opts.MaxDepth {
				continue
			}
			rels = rels[:0]
			for r := range g.Rels(cur.node, opts.Direction, opts.RelTypes...) {
				if filtering {
					if !haveFilt {
						continue
					}
					fv, ok := filtCol.Get(r.Pos)
					if !ok || fv < opts.FilterMin || fv > opts.FilterMax {
						continue
					}
				}
				v := 0.0
				if haveVal {
					v, _ = valCol.Get(r.Pos)
				}
				rels = append(rels, propRel{v, r.Neighbor})
			}
			if opts.Desc {
				sort.SliceStable(rels, func(i, j int) bool { return rels[i].value > rels[j].value })
			} else {
				sort.SliceStable(rels, func(i, j int) bool { return rels[i].value < rels[j].value })
			}
			if opts.TruncLimit > 0 && len(rels) > opts.TruncLimit {
				rels = rels[:opts.TruncLimit]
			}
			for _, r := range rels {
				if r.value > opts.MinValue && !visited[r.nbr] {
					visited[r.nbr] = true
					queue = append(queue, propHop{r.nbr, cur.depth + 1, r.value})
				}
			}
		}
	}

	out := make([]PropagateResult, 0, len(acc))
	for n, e := range acc {
		out = append(out, PropagateResult{n, e.value, e.depth})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}
