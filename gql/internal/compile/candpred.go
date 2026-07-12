// Per-candidate predicate specialization: during one bind-chain level's
// candidate iteration every row slot except the level's own is fixed, so
// a pushed-down conjunct is a unary function of the candidate id. The
// dominant conjunct shape -- the fused prop-vs-const comparison -- runs
// as a monomorphic closure reading its hoisted typed column directly,
// skipping the compiled tree's dispatch and per-candidate boxing. Every
// pairing whose comparison semantics are not trivially same-kind boxes
// the column value once and routes through value.Compare, so results are
// exactly the general path's (missing property = failed compare = Null =
// prune, for all six operators).
package compile

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// CandPred reports whether the conjunct holds for a node candidate bound
// at its specialized slot. The row carries the level's fixed outer
// bindings -- only the carried-list rebuild (once per match-call epoch)
// reads it; hot paths touch the candidate alone.
type CandPred func(ctx *eval.Ctx, row []value.Value, id graph.NodeID) bool

// CandidatePred specializes c when its only per-candidate dependency is
// the node at slot and its shape is the fused prop-vs-const comparison or
// a constant- or carried-list membership over the candidate's property
// (or over the candidate itself: a bare-variable element probes the node
// value directly, no read at all); ok=false keeps the general row
// evaluation.
func CandidatePred(c *Compiled, slot int, slots map[string]int) (CandPred, bool) {
	// inElem resolves a membership element over slot: a property read, or
	// the identity (the candidate node itself); nil declines.
	inElem := func(e cnode) func(id graph.NodeID) value.Value {
		switch n := e.(type) {
		case *cProp:
			if n.slot != slot {
				return nil
			}
			reader := n.reader
			return reader.readNode
		case *cSlot:
			if n.s != slot {
				return nil
			}
			return func(id graph.NodeID) value.Value { return value.Node(id) }
		}
		return nil
	}
	// bareSlot reports whether the membership element IS the candidate
	// itself -- then a node-id membership probes the sorted id set with
	// the raw candidate: no boxing, no resultFor dispatch. (memNodes is
	// built from an all-node list, so hasNull is impossible; a miss
	// prunes either way under the predicate's truthy collapse.)
	bareSlot := func(e cnode) bool {
		s, ok := e.(*cSlot)
		return ok && s.s == slot
	}
	if in, ok := c.c.(*cInConst); ok {
		read := inElem(in.e)
		if read == nil {
			return nil, false
		}
		if bareSlot(in.e) && in.m.kind == memNodes {
			m := &in.m
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				return m.hasNode(uint32(id))
			}, true
		}
		m := &in.m
		return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
			v := read(id)
			if v.IsNull() {
				return false
			}
			return m.resultFor(v).IsTruthy()
		}, true
	}
	if in, ok := c.c.(*cInCarried); ok {
		read := inElem(in.e)
		if read == nil {
			return nil, false
		}
		bare := bareSlot(in.e)
		g := c.g
		return func(ctx *eval.Ctx, row []value.Value, id graph.NodeID) bool {
			in.refresh(ctx, g, row, slots)
			if in.notList {
				return false
			}
			// The carried list may change per epoch; re-check the
			// representation per call (one byte compare).
			if bare && in.m.kind == memNodes {
				return in.m.hasNode(uint32(id))
			}
			v := read(id)
			if v.IsNull() {
				return false
			}
			return in.m.resultFor(v).IsTruthy()
		}, true
	}
	n, ok := c.c.(*cCmpPropConst)
	if !ok || n.prop.slot != slot {
		return nil, false
	}
	reader := n.prop.reader
	keep := opKeep(n.op)
	rev := n.rev
	konst := n.c
	// Same-kind numeric pairs compare unboxed -- through float64 exactly
	// like value.Compare's asNum path (Int-vs-Int also routes through
	// float64 there; mirroring is the invariant, not improving on it).
	// Everything else boxes the column read once and uses the shared
	// kernel.
	if reader.node.kind == colI64 && konst.Kind() == value.KindInt {
		ci, _ := konst.AsInt()
		cf := float64(ci)
		return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
			v, present := reader.node.i64.Get(uint32(id))
			if !present {
				return false
			}
			a, b := float64(v), cf
			if rev {
				a, b = cf, float64(v)
			}
			o, comparable := cmpFloat(a, b)
			return comparable && keep(o)
		}, true
	}
	if reader.node.kind == colF64 && konst.Kind() == value.KindFloat {
		cf, _ := konst.AsFloat()
		return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
			v, present := reader.node.f64.Get(uint32(id))
			if !present {
				return false
			}
			a, b := v, cf
			if rev {
				a, b = cf, v
			}
			o, comparable := cmpFloat(a, b)
			return comparable && keep(o)
		}, true
	}
	return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
		l, r := reader.readNode(id), konst
		if rev {
			l, r = r, l
		}
		o, comparable := value.Compare(l, r)
		return comparable && keep(o)
	}, true
}

// CandBatch sweeps a whole candidate buffer for one conjunct: it clears
// keep[i] for every candidate that fails, touching only still-kept
// entries. The batch form exists for shapes whose per-candidate work is
// pure array arithmetic -- no closure call, no dispatch per element.
type CandBatch func(ctx *eval.Ctx, row []value.Value, cand []graph.NodeID, keep []bool)

// CandidateBatch specializes c to a buffer sweep when its shape allows
// the columnar form: today the fused prop-vs-const comparison over an
// i64 column with a contiguous-presence window (SliceRange), where the
// filter reduces to vals[id-start] compared against a pre-encoded
// constant -- the dominant pushed-down filter shape over LDBC-style
// label-block columns. Semantics mirror the scalar predicate exactly
// (value.Compare's float64 partial order; out-of-window = absent =
// prune). ok=false keeps the scalar path.
func CandidateBatch(c *Compiled, slot int) (CandBatch, bool) {
	n, ok := c.c.(*cCmpPropConst)
	if !ok || n.prop.slot != slot {
		return nil, false
	}
	col := n.prop.reader.node
	if col.kind != colI64 || n.c.Kind() != value.KindInt {
		return nil, false
	}
	start, vals, windowed := col.i64.SliceRange()
	if !windowed {
		return nil, false
	}
	ci, _ := n.c.AsInt()
	cf := float64(ci)
	keepOrd := opKeep(n.op)
	rev := n.rev
	return func(_ *eval.Ctx, _ []value.Value, cand []graph.NodeID, keep []bool) {
		for i, id := range cand {
			if !keep[i] {
				continue
			}
			w := uint32(id) - start
			if w >= uint32(len(vals)) {
				keep[i] = false
				continue
			}
			a, b := float64(vals[w]), cf
			if rev {
				a, b = cf, float64(vals[w])
			}
			o, comparable := cmpFloat(a, b)
			if !comparable || !keepOrd(o) {
				keep[i] = false
			}
		}
	}, true
}

// opKeep is the six comparison operators' order acceptance.
func opKeep(op ast.BinOp) func(int) bool {
	switch op {
	case ast.OpEq:
		return func(o int) bool { return o == 0 }
	case ast.OpNeq:
		return func(o int) bool { return o != 0 }
	case ast.OpLt:
		return func(o int) bool { return o < 0 }
	case ast.OpLte:
		return func(o int) bool { return o <= 0 }
	case ast.OpGt:
		return func(o int) bool { return o > 0 }
	default: // ast.OpGte
		return func(o int) bool { return o >= 0 }
	}
}

// cmpFloat is value.Compare's float64 partial order: NaN on either side
// is incomparable.
func cmpFloat(a, b float64) (int, bool) {
	switch {
	case a < b:
		return -1, true
	case a > b:
		return 1, true
	case a == b:
		return 0, true
	}
	return 0, false
}
