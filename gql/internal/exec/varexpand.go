// Quantified (variable-length) expansion: bounded enumeration walks
// relationship-unique trails (one row per path, GQL TRAIL semantics);
// zero-length or unbounded quantifiers resolve the distinct reachable set
// via a dedup'd BFS so a cyclic walk terminates. Each walk mode threads
// the same hopGate seam: a stateless per-hop predicate and a stateful
// carry+accept constraint prune during the walk.
package exec

import (
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
// of a one-element row per traversed relationship. The row is a reused
// scratch on the filter -- a fresh slice per keep would allocate once per
// traversed relationship, and the gate is compiled per execution so the
// scratch is never shared.
type hopFilter struct {
	eval  RowEval
	scope map[string]int
	srow  [1]value.Value
}

func (h *hopFilter) keep(ctx *eval.Ctx, pos uint32) bool {
	h.srow[0] = value.Rel(pos)
	return h.eval.Eval(ctx, h.srow[:], h.scope).IsTruthy()
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
	// srow is the reused one-element eval row (see hopFilter.srow): step
	// runs once per candidate hop, so a per-call slice would allocate on
	// every traversed relationship of the walk.
	srow [1]value.Value
}

// step consumes one hop: the returned state carries the hop's value, and
// ok reports whether the hop may follow the previous state.
func (h *hopCarry) step(ctx *eval.Ctx, pos uint32, st carryState) (carryState, bool) {
	h.srow[0] = value.Rel(pos)
	v := h.eval.Eval(ctx, h.srow[:], h.scope)
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
func varExpandCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, gate hopGate, row []value.Value, uniq *uniqEnv, cand *[]graph.NodeID, relData *[]uint32, relRanges *[][2]int, pairData *[][2]graph.NodeID, pairRanges *[][2]int, scratch *genScratch) {
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
	// A tracked op with LATER intersecting ops must expose each
	// candidate's trail pairs so they can be pushed for those ops to check
	// -- which forces the reach-shaped walk below into per-trail
	// enumeration.
	collectPairs := op.Uniq != nil && op.Uniq.Contribute
	// Zero-length / unbounded quantifiers resolve the distinct reachable
	// set via dedup'd BFS, so an unbounded or cyclic walk can't explode
	// into per-path enumeration. Excluding the scope's used pairs during
	// the BFS keeps the endpoint SET exact: any walk in the pair-filtered
	// graph reduces to a trail avoiding those rels and vice versa.
	if (op.Min == 0 || op.Max == nil) && !collectPairs {
		// Chain collapse: a zero-minimum unbounded reach over a
		// functional rel type whose target label only chain terminals
		// carry is ONE root-array lookup -- the structural facts are
		// verified against the data and cached by the engine
		// (ChainCollapseVia), and the per-op resolution is cached per
		// execution. Per-hop predicates and rel-uniqueness checks keep
		// the general walk.
		if op.Min == 0 && op.Max == nil && gate.pred == nil &&
			(op.Uniq == nil || !op.Uniq.Check) && len(op.Labels) > 0 {
			if roots, ok := scratch.chainRootsFor(ctx, op); ok {
				root := start
				if int(start) < len(roots) {
					root = roots[start]
				}
				if (!haveBound || bound == root) && ctx.G.NodeMatcherAccepts(m, root) {
					*cand = append(*cand, root)
				}
				return
			}
		}
		// Functional chain walk: a functional type's reachable set is its
		// ancestor chain, so the general BFS's frontier and dedup maps
		// reduce to following the single rel -- honoring the same per-hop
		// predicate and rel-uniqueness checks per step. Falls back to the
		// BFS past chainWalkMax steps (a cycle in a functional type, or a
		// pathological chain).
		if scratch.chainFuncFor(ctx, op) &&
			chainReach(ctx, start, op, m, rm, gate, bound, haveBound, uniq, cand, &scratch.reach) {
			return
		}
		varReach(ctx, start, op, m, rm, gate, bound, haveBound, uniq, cand, &scratch.reach)
		return
	}
	// Contributing reach-shaped walks enumerate trails instead (finite:
	// a trail never repeats an edge pair). Min 0 also emits the
	// zero-length walk -- the start node itself, using no relationships.
	maxHops := unboundedHops
	if op.Max != nil {
		maxHops = *op.Max
	}
	// Reset the persistent walk in place: the buffer fields keep their
	// backing arrays across rows.
	w := &scratch.walk
	*w = varWalk{
		ctx: ctx, op: op, m: m, rm: rm, gate: gate,
		bound: bound, haveBound: haveBound,
		collectRels:  op.RelSlot != plan.NoSlot,
		collectPairs: collectPairs,
		uniq:         uniq,
		max:          maxHops,
		cand:         cand, relData: relData, relRanges: relRanges,
		pairData: pairData, pairRanges: pairRanges,
		pathRels: w.pathRels[:0], pathPairs: w.pathPairs[:0], used: w.used[:0], visited: w.visited[:0],
		scratch: w.scratch, nbufN: w.nbufN[:0], nbufP: w.nbufP[:0],
	}
	if op.Acyclic {
		w.visited = append(w.visited, start)
	}
	if op.Min == 0 && (!haveBound || bound == start) && ctx.G.NodeMatcherAccepts(m, start) {
		*cand = append(*cand, start)
		if w.collectRels {
			*relRanges = append(*relRanges, [2]int{len(*relData), 0})
		}
		if collectPairs {
			*pairRanges = append(*pairRanges, [2]int{len(*pairData), 0})
		}
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
// chainWalkMax bounds the functional chain walk; a longer chain (or a
// cycle, which a functional type can still form) falls back to the
// dedup'd BFS. Real forest chains -- reply threads, org hierarchies --
// sit far below it.
const chainWalkMax = 64

// chainReach is varReach for a FUNCTIONAL rel type: the reachable set is
// the ancestor chain, so it follows the single rel per node with no
// frontier or dedup state, applying the same bound/matcher acceptance,
// hop-gate predicate, and rel-uniqueness exclusion per step. Reports
// false (leaving out untouched) when the chain exceeds chainWalkMax --
// the caller then runs the general walk.
func chainReach(ctx *eval.Ctx, start graph.NodeID, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, gate hopGate, bound graph.NodeID, haveBound bool, uniq *uniqEnv, out *[]graph.NodeID, rs *reachScratch) bool {
	cap := uint64(1<<63 - 1)
	if op.Max != nil {
		cap = *op.Max
	}
	keep := func(nb graph.NodeID) bool {
		return (!haveBound || bound == nb) && ctx.G.NodeMatcherAccepts(m, nb)
	}
	edgeOK := func(u, nb graph.NodeID) bool {
		if op.Uniq == nil || !op.Uniq.Check {
			return true
		}
		a, b := uniqPair(op.Dir, u, nb)
		return !uniq.used(op.Uniq.Scope, a, b)
	}
	mark := len(*out)
	if op.Min == 0 && keep(start) {
		*out = append(*out, start)
	}
	cur := start
	for depth := uint64(0); depth < cap; {
		if depth >= chainWalkMax {
			*out = (*out)[:mark]
			return false
		}
		rs.nbuf, rs.pbuf = ctx.G.AppendRelationshipsMatched(rs.nbuf[:0], rs.pbuf[:0], cur, op.Dir, rm)
		if len(rs.nbuf) == 0 {
			return true
		}
		nb := rs.nbuf[0]
		if gate.pred != nil && !gate.pred.keep(ctx, rs.pbuf[0]) {
			return true
		}
		if !edgeOK(cur, nb) {
			return true
		}
		depth++
		// A functional chain revisiting the start has cycled; every
		// reachable node is already emitted.
		if nb == start {
			return true
		}
		if depth >= op.Min && keep(nb) {
			*out = append(*out, nb)
		}
		cur = nb
	}
	return true
}

func varReach(ctx *eval.Ctx, start graph.NodeID, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, gate hopGate, bound graph.NodeID, haveBound bool, uniq *uniqEnv, out *[]graph.NodeID, rs *reachScratch) {
	cap := uint64(1<<63 - 1)
	if op.Max != nil {
		cap = *op.Max
	}
	keep := func(nb graph.NodeID) bool {
		return (!haveBound || bound == nb) && ctx.G.NodeMatcherAccepts(m, nb)
	}
	// A checking op excludes edges whose canonical pair the scope already
	// used (see varExpandCandidates).
	edgeOK := func(u, nb graph.NodeID) bool {
		if op.Uniq == nil || !op.Uniq.Check {
			return true
		}
		a, b := uniqPair(op.Dir, u, nb)
		return !uniq.used(op.Uniq.Scope, a, b)
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
					if gate.pred.keep(ctx, rs.pbuf[i]) && edgeOK(u, nb) {
						kept = append(kept, nb)
					}
				}
				rs.nbuf = kept
			} else {
				rs.nbuf = ctx.G.AppendNeighborsMatched(rs.nbuf[:0], u, op.Dir, rm)
				if op.Uniq != nil && op.Uniq.Check {
					kept := rs.nbuf[:0]
					for _, nb := range rs.nbuf {
						if edgeOK(u, nb) {
							kept = append(kept, nb)
						}
					}
					rs.nbuf = kept
				}
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

// unboundedHops is the trail walk's cap for an unbounded quantifier under
// pair collection (the walk stays finite: a trail never repeats a pair).
const unboundedHops = ^uint64(0)
