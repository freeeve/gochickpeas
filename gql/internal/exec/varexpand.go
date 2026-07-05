// Quantified (variable-length) expansion: bounded enumeration walks
// relationship-unique trails (one row per path, GQL TRAIL semantics);
// zero-length or unbounded quantifiers resolve the distinct reachable set
// via a dedup'd BFS so a cyclic walk terminates. Each walk mode threads
// the same hopGate seam: a stateless per-hop predicate and a stateful
// carry+accept constraint prune during the walk.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// hopGate bundles a var-expand op's compiled per-hop constraints: an
// optional stateless relationship predicate and an optional stateful
// carry. It is the one seam every walk mode shares, so a future
// carried-state constraint (sum-bounded, non-decreasing weight) extends
// hopCarry instead of threading another field and parameter set.
type hopGate struct {
	pred  *hopFilter
	carry *hopCarry
}

// hopFilter is a compiled per-hop relationship predicate lifted from
// all(r IN rels(e) WHERE pred): the iteration variable reads from slot 0
// of a one-element row per traversed relationship.
type hopFilter struct {
	eval  RowEval
	scope map[string]int
}

func (h *hopFilter) keep(ctx *eval.Ctx, pos uint32) bool {
	return h.eval.Eval(ctx, []value.Value{value.Rel(pos)}, h.scope).IsTruthy()
}

// carryState is the opaque per-path state a walk threads hop to hop for
// its gate's carry; the zero value means "no state yet" (a path's first
// hop has nothing to compare against).
type carryState struct {
	val  value.Value
	have bool
}

// hopCarry is the stateful per-hop constraint: step reads the candidate
// hop's carried value and either extends the path's state or rejects the
// hop. The monotonic-key constraint is the only instance today -- state
// is the previous hop's key, compared with the filter's own three-valued
// value.Compare so the pruned set exactly matches the source filter: an
// incomparable pair (missing key, NaN, mixed kinds) fails an all()-shape
// comparison and prunes, but is not a violation in the violation-count
// shape and passes (nullsPass). Carry is meaningless for the reachability
// BFS (dedup collapses paths, so there is no per-path state); the planner
// never attaches a spec to a min-0/unbounded op.
type hopCarry struct {
	eval      RowEval
	scope     map[string]int
	ascending bool
	nullsPass bool
}

// step consumes one hop: the returned state carries the hop's value, and
// ok reports whether the hop may follow the previous state.
func (h *hopCarry) step(ctx *eval.Ctx, pos uint32, st carryState) (carryState, bool) {
	v := h.eval.Eval(ctx, []value.Value{value.Rel(pos)}, h.scope)
	if st.have && !h.allows(st.val, v) {
		return st, false
	}
	return carryState{val: v, have: true}, true
}

// allows reports whether a hop whose key is cur may follow a hop whose key
// is prev under the spec's order and null semantics.
func (h *hopCarry) allows(prev, cur value.Value) bool {
	c, ok := value.Compare(prev, cur)
	if !ok {
		return h.nullsPass
	}
	if h.ascending {
		return c < 0
	}
	return c > 0
}

// buildHopGates compiles each var-length op's per-hop constraints once for
// the stage, indexed by op (the per-row form allocated a scope map and a
// compiled tree on every row entering the walk).
func buildHopGates(ctx *eval.Ctx, ops []plan.BindOp) []hopGate {
	out := make([]hopGate, len(ops))
	for i := range ops {
		if ops[i].Kind != plan.OpVarExpand {
			continue
		}
		if rp := ops[i].RelPred; rp != nil {
			scope := map[string]int{rp.Var: 0}
			out[i].pred = &hopFilter{eval: compileEval(ctx, rp.Pred, scope), scope: scope}
		}
		if mh := ops[i].MonoHop; mh != nil {
			scope := map[string]int{"__r": 0}
			out[i].carry = &hopCarry{
				eval:      compileEval(ctx, &ast.Prop{Var: "__r", Key: mh.RelKey}, scope),
				scope:     scope,
				ascending: mh.Ascending,
				nullsPass: mh.NullsPass,
			}
		}
	}
	return out
}

// varExpandCandidates fills the candidate buffers for a quantified hop:
// endpoint nodes, and for a named rel variable the flat rel arena plus
// per-candidate (start, len) ranges. The walk state and dedup set live on
// the stage's genScratch, reused across the row loop (the walk runs to
// completion per call and is never nested, same as reachScratch).
func varExpandCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, gate hopGate, row []value.Value, cand *[]graph.NodeID, relData *[]uint32, relRanges *[][2]int, scratch *genScratch) {
	start, ok := row[op.From].AsNode()
	if !ok {
		return
	}
	var bound graph.NodeID
	haveBound := false
	if op.Rebind {
		bound, haveBound = row[op.To].AsNode()
		if !haveBound {
			return
		}
	}
	// Zero-length / unbounded quantifiers resolve the distinct reachable
	// set via dedup'd BFS, so an unbounded or cyclic walk can't explode
	// into per-path enumeration.
	if op.Min == 0 || op.Max == nil {
		varReach(ctx, start, op, m, rm, gate, bound, haveBound, cand, &scratch.reach)
		return
	}
	// Reset the persistent walk in place: the buffer fields keep their
	// backing arrays across rows.
	w := &scratch.walk
	*w = varWalk{
		ctx: ctx, op: op, m: m, rm: rm, gate: gate,
		bound: bound, haveBound: haveBound,
		collectRels: op.RelSlot != plan.NoSlot,
		max:         *op.Max,
		cand:        cand, relData: relData, relRanges: relRanges,
		pathRels: w.pathRels[:0], used: w.used[:0], visited: w.visited[:0],
		scratch: w.scratch, nbufN: w.nbufN[:0], nbufP: w.nbufP[:0],
	}
	if op.Acyclic {
		w.visited = append(w.visited, start)
	}
	w.dfs(start, 0, carryState{})
	// Without a named rel variable the trail's positions are not bound;
	// only then does endpoint dedup apply (first-seen order preserved).
	if !w.collectRels {
		if op.DedupEndpoints {
			if scratch.dedup == nil {
				scratch.dedup = make(map[graph.NodeID]struct{}, len(*cand))
			} else {
				clear(scratch.dedup)
			}
			kept := (*cand)[:0]
			for _, n := range *cand {
				if _, dup := scratch.dedup[n]; !dup {
					scratch.dedup[n] = struct{}{}
					kept = append(kept, n)
				}
			}
			*cand = kept
		}
		*relData = (*relData)[:0]
		*relRanges = (*relRanges)[:0]
	}
}

// varReach is the distinct nodes reachable in min..=max hops (max nil =
// unbounded): a dedup'd BFS binding each reachable endpoint once. min 0
// includes the start itself. expanded bounds the walk (each node's
// neighbors explored once, so cycles terminate); emitted dedups the result
// separately so a node re-reached via a cycle at depth >= min -- e.g. the
// start when min >= 1 -- still emits once. The gate's stateless predicate
// prunes edges here too; its carry never applies (see hopCarry).
func varReach(ctx *eval.Ctx, start graph.NodeID, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, gate hopGate, bound graph.NodeID, haveBound bool, out *[]graph.NodeID, rs *reachScratch) {
	cap := uint64(1<<63 - 1)
	if op.Max != nil {
		cap = *op.Max
	}
	keep := func(nb graph.NodeID) bool {
		return (!haveBound || bound == nb) && ctx.G.NodeMatcherAccepts(m, nb)
	}
	// Reuse the stage's working sets: clear the dedup maps and reset the
	// frontier/neighbor buffers rather than allocating them per row.
	if rs.expanded == nil {
		rs.expanded = map[graph.NodeID]struct{}{}
		rs.emitted = map[graph.NodeID]struct{}{}
	} else {
		clear(rs.expanded)
		clear(rs.emitted)
	}
	rs.expanded[start] = struct{}{}
	if op.Min == 0 && keep(start) {
		rs.emitted[start] = struct{}{}
		*out = append(*out, start)
	}
	frontier := append(rs.frontier[:0], start)
	next := rs.next[:0]
	for depth := uint64(0); len(frontier) > 0 && depth < cap; depth++ {
		d := depth + 1
		next = next[:0]
		for _, u := range frontier {
			if gate.pred != nil {
				// The predicate needs rel positions: walk relationships and
				// keep the passing edges' endpoints.
				rs.nbuf, rs.pbuf = ctx.G.AppendRelationshipsMatched(rs.nbuf[:0], rs.pbuf[:0], u, op.Dir, rm)
				kept := rs.nbuf[:0]
				for i, nb := range rs.nbuf {
					if gate.pred.keep(ctx, rs.pbuf[i]) {
						kept = append(kept, nb)
					}
				}
				rs.nbuf = kept
			} else {
				rs.nbuf = ctx.G.AppendNeighborsMatched(rs.nbuf[:0], u, op.Dir, rm)
			}
			for _, nb := range rs.nbuf {
				if d >= op.Min && keep(nb) {
					if _, dup := rs.emitted[nb]; !dup {
						rs.emitted[nb] = struct{}{}
						*out = append(*out, nb)
					}
				}
				if _, seen := rs.expanded[nb]; !seen {
					rs.expanded[nb] = struct{}{}
					next = append(next, nb)
				}
			}
		}
		// Swap the two persistent buffers: next becomes the frontier, the
		// old frontier is reused as next's backing.
		frontier, next = next, frontier
	}
	rs.frontier, rs.next = frontier, next
}

// varWalk is the bounded trail enumeration's state: emitted endpoints (one
// per qualifying trail), the named-rel arena, the relationship-uniqueness
// edge stack, and per-depth candidate buffers reused across siblings.
type varWalk struct {
	ctx         *eval.Ctx
	op          *plan.BindOp
	m           *graph.NodeMatcher
	rm          *graph.RelMatcher
	gate        hopGate
	bound       graph.NodeID
	haveBound   bool
	collectRels bool
	max         uint64

	pathRels []uint32
	used     [][2]graph.NodeID
	// visited is the ACYCLIC mode's node stack (seeded with the start
	// node); empty means trail semantics (rel uniqueness only).
	visited []graph.NodeID
	scratch [][]nodePos

	cand      *[]graph.NodeID
	relData   *[]uint32
	relRanges *[][2]int

	// nbufN/nbufP are the per-hop batch buffers (parallel neighbor/pos),
	// reused across dfs levels: each level copies them into its own
	// scratch before recursing.
	nbufN []graph.NodeID
	nbufP []uint32
}

type nodePos struct {
	nb  graph.NodeID
	pos uint32
}

// dfs enumerates trails of length min..=max with no repeated relationship,
// emitting each qualifying endpoint (one entry per trail, matching the
// per-path semantics). st is the gate carry's per-path state, threaded
// down the recursion.
func (w *varWalk) dfs(cur graph.NodeID, depth uint64, st carryState) {
	if depth >= w.max {
		return
	}
	d := int(depth)
	for len(w.scratch) <= d {
		w.scratch = append(w.scratch, nil)
	}
	buf := w.scratch[d][:0]
	// Walk relationships (carrying positions) when the gate prunes by
	// position or the rel list is recorded; otherwise the leaner neighbor
	// batch. Batch appends replace the iter.Seq forms, which heap-allocate
	// their closures per call.
	if w.gate.pred != nil || w.gate.carry != nil || w.collectRels {
		w.nbufN, w.nbufP = w.ctx.G.AppendRelationshipsMatched(w.nbufN[:0], w.nbufP[:0], cur, w.op.Dir, w.rm)
		for i, nb := range w.nbufN {
			if p := w.nbufP[i]; w.gate.pred == nil || w.gate.pred.keep(w.ctx, p) {
				buf = append(buf, nodePos{nb, p})
			}
		}
	} else {
		w.nbufN = w.ctx.G.AppendNeighborsMatched(w.nbufN[:0], cur, w.op.Dir, w.rm)
		for _, nb := range w.nbufN {
			buf = append(buf, nodePos{nb, 0})
		}
	}
	w.scratch[d] = buf
	for i := 0; i < len(w.scratch[d]); i++ {
		nb, pos := w.scratch[d][i].nb, w.scratch[d][i].pos
		// Relationship-uniqueness: undirected edges are unordered.
		edge := [2]graph.NodeID{cur, nb}
		if w.op.Dir == graph.Both && nb < cur {
			edge = [2]graph.NodeID{nb, cur}
		}
		if containsEdge(w.used, edge) {
			continue
		}
		// ACYCLIC additionally rejects a hop that revisits any node on
		// this path (the start included).
		if w.op.Acyclic && containsNode(w.visited, nb) {
			continue
		}
		// Carried-state pruning: a hop the carry rejects (e.g. its key does
		// not continue the monotonic order) admits no qualifying trail
		// through it. A path's first hop starts the state, never fails it.
		next := st
		if w.gate.carry != nil {
			var ok bool
			if next, ok = w.gate.carry.step(w.ctx, pos, st); !ok {
				continue
			}
		}
		w.used = append(w.used, edge)
		if w.op.Acyclic {
			w.visited = append(w.visited, nb)
		}
		if w.collectRels {
			w.pathRels = append(w.pathRels, pos)
		}
		nd := depth + 1
		if nd >= w.op.Min && (!w.haveBound || w.bound == nb) && w.ctx.G.NodeMatcherAccepts(w.m, nb) {
			*w.cand = append(*w.cand, nb)
			if w.collectRels {
				*w.relRanges = append(*w.relRanges, [2]int{len(*w.relData), len(w.pathRels)})
				*w.relData = append(*w.relData, w.pathRels...)
			}
		}
		w.dfs(nb, nd, next)
		if w.collectRels {
			w.pathRels = w.pathRels[:len(w.pathRels)-1]
		}
		if w.op.Acyclic {
			w.visited = w.visited[:len(w.visited)-1]
		}
		w.used = w.used[:len(w.used)-1]
	}
}

// containsNode is the acyclic stack's membership test -- same short
// linear scan rationale as containsEdge.
func containsNode(visited []graph.NodeID, n graph.NodeID) bool {
	return slices.Contains(visited, n)
}

// containsEdge is the trail stack's membership test -- a short linear scan
// (never longer than max, e.g. 3 for {1,3}) beats a hashing set.
func containsEdge(used [][2]graph.NodeID, e [2]graph.NodeID) bool {
	return slices.Contains(used, e)
}
