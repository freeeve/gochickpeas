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
// at its specialized slot.
type CandPred func(ctx *eval.Ctx, id graph.NodeID) bool

// CandidatePred specializes c when its only row dependency is the node
// candidate at slot and its shape is the fused prop-vs-const comparison
// or a constant-list membership over the candidate's property; ok=false
// keeps the general row evaluation.
func CandidatePred(c *Compiled, slot int) (CandPred, bool) {
	if in, ok := c.c.(*cInConst); ok {
		p, isProp := in.e.(*cProp)
		if !isProp || p.slot != slot {
			return nil, false
		}
		reader := p.reader
		m := in.m
		return func(_ *eval.Ctx, id graph.NodeID) bool {
			v := reader.readNode(id)
			if v.IsNull() {
				return false
			}
			return m.resultFor(v).IsTruthy()
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
		return func(_ *eval.Ctx, id graph.NodeID) bool {
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
		return func(_ *eval.Ctx, id graph.NodeID) bool {
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
	return func(_ *eval.Ctx, id graph.NodeID) bool {
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
