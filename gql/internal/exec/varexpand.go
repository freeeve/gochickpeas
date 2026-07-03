// Quantified (variable-length) expansion: bounded enumeration walks
// relationship-unique trails (one row per path, GQL TRAIL semantics);
// zero-length or unbounded quantifiers resolve the distinct reachable set
// via a dedup'd BFS so a cyclic walk terminates. Per-hop predicates and
// the monotonic-key constraint prune during the walk.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

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

// buildHopFilters compiles each var-length op's per-hop predicate once for
// the stage, indexed by op.
func buildHopFilters(ctx *eval.Ctx, ops []plan.BindOp) []*hopFilter {
	out := make([]*hopFilter, len(ops))
	for i := range ops {
		if ops[i].Kind == plan.OpVarExpand && ops[i].RelPred != nil {
			rp := ops[i].RelPred
			scope := map[string]int{rp.Var: 0}
			out[i] = &hopFilter{eval: compileEval(ctx, rp.Pred, scope), scope: scope}
		}
	}
	return out
}

// monoFilter reads each candidate hop's i64 key so the DFS can prune a
// hop that doesn't strictly continue the order vs the previous hop.
type monoFilter struct {
	eval      RowEval
	scope     map[string]int
	ascending bool
}

func (m *monoFilter) value(ctx *eval.Ctx, pos uint32) (int64, bool) {
	return m.eval.Eval(ctx, []value.Value{value.Rel(pos)}, m.scope).AsInt()
}

// varExpandCandidates fills the candidate buffers for a quantified hop:
// endpoint nodes, and for a named rel variable the flat rel arena plus
// per-candidate (start, len) ranges.
func varExpandCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, hop *hopFilter, row []value.Value, cand *[]graph.NodeID, relData *[]uint32, relRanges *[][2]int) {
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
		varReach(ctx, start, op, m, rm, bound, haveBound, cand)
		return
	}
	w := &varWalk{
		ctx: ctx, op: op, m: m, hop: hop,
		bound: bound, haveBound: haveBound,
		collectRels: op.RelSlot != plan.NoSlot,
		max:         *op.Max,
		cand:        cand, relData: relData, relRanges: relRanges,
	}
	if op.Acyclic {
		w.visited = append(w.visited, start)
	}
	if op.MonoHop != nil {
		scope := map[string]int{"__r": 0}
		w.mono = &monoFilter{
			eval:      compileEval(ctx, &ast.Prop{Var: "__r", Key: op.MonoHop.RelKey}, scope),
			scope:     scope,
			ascending: op.MonoHop.Ascending,
		}
	}
	w.dfs(start, 0, 0, false)
	// Without a named rel variable the trail's positions are not bound;
	// only then does endpoint dedup apply (first-seen order preserved).
	if !w.collectRels {
		if op.DedupEndpoints {
			seen := make(map[graph.NodeID]struct{}, len(*cand))
			kept := (*cand)[:0]
			for _, n := range *cand {
				if _, dup := seen[n]; !dup {
					seen[n] = struct{}{}
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
// start when min >= 1 -- still emits once.
func varReach(ctx *eval.Ctx, start graph.NodeID, op *plan.BindOp, m *graph.NodeMatcher, rm *graph.RelMatcher, bound graph.NodeID, haveBound bool, out *[]graph.NodeID) {
	cap := uint64(1<<63 - 1)
	if op.Max != nil {
		cap = *op.Max
	}
	keep := func(nb graph.NodeID) bool {
		return (!haveBound || bound == nb) && ctx.G.NodeMatcherAccepts(m, nb)
	}
	expanded := map[graph.NodeID]struct{}{start: {}}
	emitted := map[graph.NodeID]struct{}{}
	if op.Min == 0 && keep(start) {
		emitted[start] = struct{}{}
		*out = append(*out, start)
	}
	frontier := []graph.NodeID{start}
	var nbuf []graph.NodeID // batch buffer: the iter.Seq form allocates per (u) call
	for depth := uint64(0); len(frontier) > 0 && depth < cap; depth++ {
		d := depth + 1
		var next []graph.NodeID
		for _, u := range frontier {
			nbuf = ctx.G.AppendNeighborsMatched(nbuf[:0], u, op.Dir, rm)
			for _, nb := range nbuf {
				if d >= op.Min && keep(nb) {
					if _, dup := emitted[nb]; !dup {
						emitted[nb] = struct{}{}
						*out = append(*out, nb)
					}
				}
				if _, seen := expanded[nb]; !seen {
					expanded[nb] = struct{}{}
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
}

// varWalk is the bounded trail enumeration's state: emitted endpoints (one
// per qualifying trail), the named-rel arena, the relationship-uniqueness
// edge stack, and per-depth candidate buffers reused across siblings.
type varWalk struct {
	ctx         *eval.Ctx
	op          *plan.BindOp
	m           *graph.NodeMatcher
	hop         *hopFilter
	mono        *monoFilter
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
// per-path semantics).
func (w *varWalk) dfs(cur graph.NodeID, depth uint64, prevVal int64, havePrev bool) {
	if depth >= w.max {
		return
	}
	d := int(depth)
	for len(w.scratch) <= d {
		w.scratch = append(w.scratch, nil)
	}
	buf := w.scratch[d][:0]
	// Walk relationships (carrying positions) when a per-hop predicate or
	// the mono constraint prunes by position or the rel list is recorded;
	// otherwise the leaner neighbor batch. Batch appends replace the
	// iter.Seq forms, which heap-allocate their closures per call.
	if w.hop != nil || w.mono != nil || w.collectRels {
		w.nbufN, w.nbufP = w.ctx.G.AppendRelationships(w.nbufN[:0], w.nbufP[:0], cur, w.op.Dir, w.op.Types)
		for i, nb := range w.nbufN {
			if p := w.nbufP[i]; w.hop == nil || w.hop.keep(w.ctx, p) {
				buf = append(buf, nodePos{nb, p})
			}
		}
	} else {
		w.nbufN = w.ctx.G.AppendNeighborsByType(w.nbufN[:0], cur, w.op.Dir, w.op.Types)
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
		w.used = append(w.used, edge)
		if w.op.Acyclic {
			w.visited = append(w.visited, nb)
		}
		// Monotonic pruning: the hop's key must strictly continue the
		// order vs the previous hop, else this trail can't satisfy it.
		curVal, haveCur := prevVal, havePrev
		if w.mono != nil {
			v, ok := w.mono.value(w.ctx, pos)
			if !ok || (havePrev && ((w.mono.ascending && v <= prevVal) || (!w.mono.ascending && v >= prevVal))) {
				w.used = w.used[:len(w.used)-1]
				continue
			}
			curVal, haveCur = v, true
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
		w.dfs(nb, nd, curVal, haveCur)
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
