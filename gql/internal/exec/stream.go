// The push pipeline: a segment's stages run as a chain of row sinks, each
// holding one reused full-width row buffer, so matched rows flow through
// the chain without per-row allocation. Only the terminal sink retains
// rows (in chunked arenas); the shortest-path stage keeps its batch
// semantics behind a buffering barrier. Depth-first push order equals the
// former materialize-then-iterate order, so group encounter order,
// DISTINCT first-occurrence, and stable-sort ties are unchanged.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// rowSink consumes full-width rows pushed through a segment's stage chain.
// A pushed row is valid only for the duration of the call; a sink that
// retains it must copy. close flushes any buffered state downstream.
type rowSink interface {
	push(row []value.Value)
	close()
}

// arenaChunkValues is the bump-arena chunk size in values (not rows).
const arenaChunkValues = 16384

// rowArena bump-allocates fixed-width rows out of large chunks, so
// retaining n rows costs n/chunk allocations instead of n.
type rowArena struct {
	width int
	chunk []value.Value
	off   int
}

// alloc returns the next zeroed width-wide row.
func (a *rowArena) alloc() []value.Value {
	if a.off+a.width > len(a.chunk) {
		n := max(arenaChunkValues, a.width)
		a.chunk = make([]value.Value, n)
		a.off = 0
	}
	r := a.chunk[a.off : a.off+a.width : a.off+a.width]
	a.off += a.width
	return r
}

// rollback releases the arena's most recent alloc (a DISTINCT duplicate).
func (a *rowArena) rollback() {
	a.off -= a.width
}

// copyRow retains a transient row in the arena.
func (a *rowArena) copyRow(row []value.Value) []value.Value {
	r := a.alloc()
	copy(r, row)
	return r
}

// matchSink runs one MATCH stage per pushed row: genMatches walks the bind
// chain over the stage-local buffer, forwarding each bound match. A named
// path is assembled and post-path-filtered per match; an OPTIONAL stage
// that produced nothing re-emits the input row from the orig buffer.
type matchSink struct {
	ctx         *eval.Ctx
	stage       *plan.MatchStage
	comp        *stageComp
	slots       map[string]int
	buf, orig   []value.Value
	scratch     genScratch
	next        rowSink
	opRows      []uint64
	fired       bool
	pathFilters []RowEval
	// uniq is the segment chain's shared used-relationship env: chained
	// MATCH stages from one clause (comma patterns, planner splits) see
	// one stack per in-flight row.
	uniq *uniqEnv
	// emitFn is the emit method bound once, so genMatches gets the same
	// closure every push instead of a fresh method value.
	emitFn func([]value.Value)
}

func (m *matchSink) push(row []value.Value) {
	copy(m.buf, row)
	if m.stage.Optional {
		copy(m.orig, row)
		m.fired = false
		genMatches(m.ctx, m.stage.Ops, m.buf, m.comp, m.slots, m.uniq, m.emitFn, &m.scratch, m.opRows)
		if !m.fired {
			// The re-emitted row takes the path assembly and post-path
			// WHERE too, exactly like the former batch bindPaths pass.
			m.forward(m.orig)
		}
		return
	}
	genMatches(m.ctx, m.stage.Ops, m.buf, m.comp, m.slots, m.uniq, m.emitFn, &m.scratch, m.opRows)
}

// emit forwards one bound match. The OPTIONAL no-match probe counts every
// match (like the former pre-bindPaths batch check), so it flips before
// the post-path filter can prune.
func (m *matchSink) emit(r []value.Value) {
	m.fired = true
	m.forward(r)
}

// forward assembles the row's named path and applies the post-path WHERE
// conjuncts, then hands the row downstream.
func (m *matchSink) forward(r []value.Value) {
	if pb := m.stage.PathBind; pb != nil {
		rels := pathRelPositionsOf(r[pb.RelsSlot])
		var nodes []graph.NodeID
		if start, ok := r[pb.FromSlot].AsNode(); ok {
			nodes = reconstructPathNodes(m.ctx, start, rels)
		}
		r[pb.PathSlot] = value.Path(nodes, rels)
		for _, f := range m.pathFilters {
			if !f.Eval(m.ctx, r, m.slots).IsTruthy() {
				return
			}
		}
	}
	m.next.push(r)
}

func (m *matchSink) close() { m.next.close() }

// unwindSink is FOR x IN list per pushed row: a list emits one row per
// element, null emits none, any other scalar emits a single row.
type unwindSink struct {
	ctx   *eval.Ctx
	list  RowEval
	slots map[string]int
	out   int
	buf   []value.Value
	next  rowSink
	count *uint64
}

func (u *unwindSink) push(row []value.Value) {
	v := u.list.Eval(u.ctx, row, u.slots)
	if items, ok := v.AsList(); ok {
		for _, item := range items {
			copy(u.buf, row)
			u.buf[u.out] = item
			u.bump()
			u.next.push(u.buf)
		}
		return
	}
	if v.IsNull() {
		return
	}
	copy(u.buf, row)
	u.buf[u.out] = v
	u.bump()
	u.next.push(u.buf)
}

func (u *unwindSink) bump() {
	if u.count != nil {
		*u.count++
	}
}

func (u *unwindSink) close() { u.next.close() }

// callSink crosses each pushed row with a procedure's results, computed
// once at build (per-node scalar vector or index search hit-set). A
// backend without the native capability passes rows through unchanged.
type callSink struct {
	ctx    *eval.Ctx
	cs     *plan.CallStage
	values []value.Value
	// hits is the search hit-set's ascending-id iteration (nil = no hits);
	// unnamed so the nodeset iterator assigns directly.
	hits   func(yield func(graph.NodeID) bool)
	native bool
	buf    []value.Value
	next   rowSink
	count  *uint64
}

func (c *callSink) push(row []value.Value) {
	if !c.native {
		c.bump()
		c.next.push(row)
		return
	}
	if c.values != nil {
		for i, v := range c.values {
			copy(c.buf, row)
			if c.cs.NodeSlot != plan.NoSlot {
				c.buf[c.cs.NodeSlot] = value.Node(graph.NodeID(i))
			}
			if c.cs.ValueSlot != plan.NoSlot {
				c.buf[c.cs.ValueSlot] = v
			}
			c.bump()
			c.next.push(c.buf)
		}
		return
	}
	if c.hits == nil {
		return
	}
	c.hits(func(nid graph.NodeID) bool {
		copy(c.buf, row)
		if c.cs.NodeSlot != plan.NoSlot {
			c.buf[c.cs.NodeSlot] = value.Node(nid)
		}
		c.bump()
		c.next.push(c.buf)
		return true
	})
}

func (c *callSink) bump() {
	if c.count != nil {
		*c.count++
	}
}

func (c *callSink) close() { c.next.close() }

// subquerySink is CALL { } per pushed row: a correlated subquery runs the
// sub-plan seeded from the outer row's imports; an uncorrelated one runs
// once on first use and cross-joins.
type subquerySink struct {
	ctx     *eval.Ctx
	cs      *plan.CallSubqueryStage
	buf     []value.Value
	seed    []value.Value
	subRows [][]value.Value
	subRun  bool
	next    rowSink
	count   *uint64
}

func (s *subquerySink) push(row []value.Value) {
	var sub [][]value.Value
	if len(s.cs.ImportSlots) == 0 {
		if !s.subRun {
			s.subRows = runSubplan(s.ctx, s.cs.Sub, nil)
			s.subRun = true
		}
		sub = s.subRows
	} else {
		for i, slot := range s.cs.ImportSlots {
			s.seed[i] = row[slot]
		}
		sub = runSubplan(s.ctx, s.cs.Sub, s.seed)
	}
	for _, sr := range sub {
		copy(s.buf, row)
		for i, slot := range s.cs.OutSlots {
			s.buf[slot] = sr[i]
		}
		if s.count != nil {
			*s.count++
		}
		s.next.push(s.buf)
	}
}

func (s *subquerySink) close() { s.next.close() }

// spSink is the shortest-path stage's batch barrier: input rows buffer in
// an arena until close, because the batch runner memoizes a parent tree
// for sources shared by multiple rows.
type spSink struct {
	ctx   *eval.Ctx
	sp    *plan.SpStage
	rows  [][]value.Value
	arena rowArena
	next  rowSink
	count *uint64
}

func (s *spSink) push(row []value.Value) {
	s.rows = append(s.rows, s.arena.copyRow(row))
}

func (s *spSink) close() {
	out := runSPStage(s.ctx, s.sp, s.rows)
	if s.count != nil {
		*s.count = uint64(len(out))
	}
	for _, r := range out {
		s.next.push(r)
	}
	s.next.close()
}
