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
			m := &in.m
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				return m.hasNode(uint32(id))
			}, true
		}
		// A string column probing a constant list compares interned atom
		// ids: the list's atoms resolve once, a probe is one column read
		// and a small search -- no string resolution, no encoded hash key.
		if p, isProp := in.e.(*cProp); isProp && p.reader.node.kind == colStr {
			if atoms, ok := in.m.atomSet(c.g); ok {
				sc := p.reader.node.str
				return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
					aid, ok := sc.ID(uint32(id))
					if !ok || aid == 0 {
						return false
					}
					_, hit := slices.BinarySearch(atoms, aid)
					return hit
				}, true
			}
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
	// IS [NOT] NULL over the candidate's property is a pure presence
	// probe: one typed column read, no value materialization (the general
	// path resolves a string column's text just to discard it). The
	// string form mirrors the reader's empty-means-absent fold by atom
	// id, declining when a non-zero atom resolves empty (the one case
	// where the id test could diverge).
	if isn, ok := c.c.(*cIsNull); ok {
		p, isProp := isn.e.(*cProp)
		if !isProp || p.slot != slot {
			return nil, false
		}
		col := p.reader.node
		neg := isn.negated
		switch col.kind {
		case colI64:
			ic := col.i64
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				_, present := ic.Get(uint32(id))
				return present == neg
			}, true
		case colF64:
			fc := col.f64
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				_, present := fc.Get(uint32(id))
				return present == neg
			}, true
		case colBool:
			bc := col.bool
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				_, present := bc.Get(uint32(id))
				return present == neg
			}, true
		case colStr:
			if empty, ok := c.g.PropertyKey(""); ok && empty != 0 {
				return nil, false
			}
			sc := col.str
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				aid, ok := sc.ID(uint32(id))
				return (ok && aid != 0) == neg
			}, true
		}
		return nil, false
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
	// An i64 column against a temporal constant compares exactly as
	// int64 epoch millis (value.Compare's Temporal/Int case is cmpInt --
	// exact, always comparable), so the typed closure needs no float
	// detour.
	if reader.node.kind == colI64 && konst.Kind() == value.KindTemporal {
		cms, _, _ := konst.AsTemporal()
		return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
			v, present := reader.node.i64.Get(uint32(id))
			if !present {
				return false
			}
			a, b := v, cms
			if rev {
				a, b = cms, v
			}
			var o int
			switch {
			case a < b:
				o = -1
			case a > b:
				o = 1
			}
			return keep(o)
		}, true
	}
	// String equality against a constant compares interned atom ids: the
	// constant resolves through the shared atom table once, so a probe is
	// one column read and an integer compare -- no string resolution, no
	// byte comparison. Only =/<> qualify (atom ids carry no lexicographic
	// order), and both are operand-order symmetric, so rev is moot. The
	// general path folds absent, atom 0, and empty text to Null (prune);
	// equality against a non-empty constant needs no empty check (a
	// matching atom IS the constant's non-empty text), while inequality
	// declines when a non-zero atom resolves empty (only then could the
	// id test diverge from the reader's empty-means-absent fold).
	if reader.node.kind == colStr && konst.Kind() == value.KindStr &&
		(n.op == ast.OpEq || n.op == ast.OpNeq) {
		cs, _ := konst.AsStr()
		col := reader.node.str
		want, found := c.g.PropertyKey(cs)
		if n.op == ast.OpEq {
			if cs == "" || !found {
				// No stored non-empty text can equal it: always prune.
				return func(_ *eval.Ctx, _ []value.Value, _ graph.NodeID) bool {
					return false
				}, true
			}
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				aid, ok := col.ID(uint32(id))
				return ok && aid == want
			}, true
		}
		if empty, ok := c.g.PropertyKey(""); ok && empty != 0 {
			// A non-zero empty atom exists: keep the general path, whose
			// resolve-and-check folds it to absent.
			return nil, false
		}
		if cs == "" || !found {
			// Present non-empty text is unequal to it by construction.
			return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
				aid, ok := col.ID(uint32(id))
				return ok && aid != 0
			}, true
		}
		return func(_ *eval.Ctx, _ []value.Value, id graph.NodeID) bool {
			aid, ok := col.ID(uint32(id))
			return ok && aid != 0 && aid != want
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
