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
	"slices"

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
			ids := in.m.nodes
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				_, hit := slices.BinarySearch(ids, uint32(id))
				return hit
			}, true
		}
		m := in.m
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
				_, hit := slices.BinarySearch(in.m.nodes, uint32(id))
				return hit
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
