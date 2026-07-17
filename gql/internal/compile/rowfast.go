// Whole-row predicate specialization: a comparison whose sides are
// property reads over typed i64 columns (optionally shifted by a constant
// integer or months-free duration) -- the dominant fully-bound filter
// shape, e.g. a temporal ordering between two pattern variables -- runs as
// a monomorphic closure reading both columns directly, skipping the
// compiled tree's dispatch and per-row boxing. A slot-vs-slot comparison
// flattens to one shared-kernel Compare call. Rows whose slots do not
// hold nodes fall back to the full tree evaluation, so results are exactly
// the general path's for every input.
package compile

import (
	"math"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// addChecked / mulChecked are checked int64 add / multiply (comma-ok),
// mirroring eval's temporal guard so the specialization's constant fold
// declines exactly when the interpreter would overflow to Null.
func addChecked(a, b int64) (int64, bool) {
	c := a + b
	return c, (c > a) == (b > 0) || b == 0
}

func mulChecked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
	}
	c := a * b
	return c, c/b == a
}

// rowFast is a monomorphic whole-expression evaluator, result-identical
// to ceval on the same tree.
type rowFast func(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value

// i64Term reads one comparison side: an i64 node-column property,
// optionally shifted by a compile-time constant.
type i64Term struct {
	slot int
	col  chickpeas.I64Col
	mode uint8
	off  int64
	sub  bool
}

const (
	termPlain uint8 = iota
	// termDur shifts by a months-free duration's tick offset -- Arith's
	// Int +/- Duration through ApplyDuration, which for months == 0 is
	// unchecked tick addition (off carries the operator's sign).
	termDur
	// termInt shifts by an integer constant with Arith's checked Int
	// add/subtract semantics (overflow = Null).
	termInt
)

// read resolves the term against a row. state readNull propagates a Null
// operand (absent property, null slot, checked overflow) into the
// comparison's Null result; readFallback sends the whole row to the tree
// evaluation (a slot holding anything besides a node or null).
const (
	readOK uint8 = iota
	readNull
	readFallback
)

func (t *i64Term) read(row []value.Value) (int64, uint8) {
	if t.slot < 0 || t.slot >= len(row) {
		return 0, readNull
	}
	base := row[t.slot]
	if base.Kind() != value.KindNode {
		if base.IsNull() {
			return 0, readNull
		}
		return 0, readFallback
	}
	id, _ := base.AsNode()
	v, present := t.col.Get(uint32(id))
	if !present {
		return 0, readNull
	}
	switch t.mode {
	case termDur:
		// Months-free tick add, checked like eval's fixed ApplyDuration:
		// overflow is Null (the interpreter produces the same Null).
		c := v + t.off
		if (c > v) == (t.off > 0) || t.off == 0 {
			return c, readOK
		}
		return 0, readNull
	case termInt:
		// Mirror eval's checked int arithmetic exactly: overflow is Null.
		if t.sub {
			c := v - t.off
			if (c < v) == (t.off > 0) || t.off == 0 {
				return c, readOK
			}
			return 0, readNull
		}
		c := v + t.off
		if (c > v) == (t.off > 0) || t.off == 0 {
			return c, readOK
		}
		return 0, readNull
	}
	return v, readOK
}

// deriveRowFast specializes a compiled tree whose root is a comparison
// over two supported terms; nil keeps the tree evaluation.
func deriveRowFast(c cnode, g *chickpeas.Snapshot) rowFast {
	// A bare property read or slot reference -- the dominant projection-item
	// shapes, evaluated once per output column per row -- skips the tree
	// dispatch entirely: cevalProp/the slot read are self-contained, and the
	// closure binds the node directly (ceval's cases do nothing more).
	if p, ok := c.(*cProp); ok {
		return func(_ *eval.Ctx, row []value.Value, _ map[string]int) value.Value {
			return cevalProp(p, row)
		}
	}
	if sn, ok := c.(*cSlot); ok {
		s := sn.s
		return func(_ *eval.Ctx, row []value.Value, _ map[string]int) value.Value {
			return slotVal(row, s)
		}
	}
	// A bare prop-vs-const comparison is already fused to cCmpPropConst; derive
	// the monomorphic i64 compare from it so the common filter shape skips
	// value.Compare's type switch too (rev marks the const on the left).
	if cc, ok := c.(*cCmpPropConst); ok {
		if cc.prop.reader.node.kind != colI64 {
			return nil
		}
		k, exact, kok := constI64Val(cc.c)
		if !kok {
			return nil
		}
		t := i64Term{slot: cc.prop.slot, col: cc.prop.reader.node.i64}
		return propLitFast(t, k, exact, opKeep(cc.op), cc.rev, c, g)
	}
	bin, ok := c.(*cBin)
	if !ok {
		return nil
	}
	switch bin.op {
	case ast.OpEq, ast.OpNeq, ast.OpLt, ast.OpLte, ast.OpGt, ast.OpGte:
	default:
		return nil
	}
	keep := opKeep(bin.op)
	if a, aok := bin.l.(*cSlot); aok {
		if b, bok := bin.r.(*cSlot); bok {
			sa, sb := a.s, b.s
			return func(_ *eval.Ctx, row []value.Value, _ map[string]int) value.Value {
				o, comparable := value.Compare(slotVal(row, sa), slotVal(row, sb))
				if !comparable {
					return value.Null()
				}
				return value.Bool(keep(o))
			}
		}
	}
	ta, aok := i64TermOf(bin.l)
	tb, bok := i64TermOf(bin.r)
	if aok && bok {
		return func(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value {
			va, sa := ta.read(row)
			if sa == readFallback {
				return ceval(ctx, c, g, row, slots)
			}
			vb, sb := tb.read(row)
			if sb == readFallback {
				return ceval(ctx, c, g, row, slots)
			}
			if sa == readNull || sb == readNull {
				return value.Null()
			}
			// Same-kind ints compare through float64 exactly like
			// value.Compare's asNum path -- mirroring is the invariant.
			o, comparable := cmpFloat(float64(va), float64(vb))
			if !comparable {
				return value.Null()
			}
			return value.Bool(keep(o))
		}
	}
	// One typed term against a foldable i64 literal -- WHERE p.age > 30 /
	// WHERE m.creationDate < datetime(...), the most common filter shape, which
	// the two-term derivation above silently declined.
	if aok {
		if k, exact, kok := constI64(bin.r); kok {
			return propLitFast(ta, k, exact, keep, false, c, g)
		}
	}
	if bok {
		if k, exact, kok := constI64(bin.l); kok {
			return propLitFast(tb, k, exact, keep, true, c, g)
		}
	}
	return nil
}

// constI64 folds a bare comparison literal to the int64 the monomorphic path
// compares against, reporting whether value.Compare orders it EXACTLY (int64)
// rather than through the float64 asNum path. Only Int and Temporal literals
// fold: an Int compares Int-vs-Int through asNum (exact=false, matching the
// two-term path and the always-Int column read); a Temporal compares against
// the i64 epoch-millis column through cmpInt (exact=true), so folding it to its
// millis is bit-identical. Float/Null/Str/Bool/Duration are declined -- an
// integer compare cannot reproduce their semantics (truncating a float would
// silently change the result).
func constI64(c cnode) (k int64, exact, ok bool) {
	lit, isLit := c.(*cLit)
	if !isLit {
		return 0, false, false
	}
	return constI64Val(lit.v)
}

// constI64Val is constI64's fold over an already-extracted value -- the form
// cCmpPropConst needs, whose folded constant is a value.Value rather than a
// cLit node. Same Int/Temporal-only rule; see constI64.
func constI64Val(v value.Value) (k int64, exact, ok bool) {
	if n, isInt := v.AsInt(); isInt {
		return n, false, true
	}
	if ms, _, isTemporal := v.AsTemporal(); isTemporal {
		return ms, true, true
	}
	return 0, false, false
}

// propLitFast is the one-sided fast path: a typed i64 term compared to a folded
// constant -- one column read and an integer compare per row. exact selects the
// comparison value.Compare uses for the literal's kind (int64 for a Temporal,
// float64 asNum for an Int); litOnLeft preserves operand order so
// datetime(...) < m.creationDate is not silently inverted.
func propLitFast(t i64Term, k int64, exact bool, keep func(int) bool, litOnLeft bool, c cnode, g *chickpeas.Snapshot) rowFast {
	return func(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value {
		v, s := t.read(row)
		if s == readFallback {
			return ceval(ctx, c, g, row, slots)
		}
		if s == readNull {
			return value.Null()
		}
		lo, ro := v, k
		if litOnLeft {
			lo, ro = k, v
		}
		var o int
		comparable := true
		if exact {
			o = cmpI64(lo, ro)
		} else {
			o, comparable = cmpFloat(float64(lo), float64(ro))
		}
		if !comparable {
			return value.Null()
		}
		return value.Bool(keep(o))
	}
}

// cmpI64 is the exact int64 ordering value.Compare uses for temporal operands.
func cmpI64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// i64TermOf resolves a comparison side: a bare i64-column property read,
// or one shifted by a constant integer or months-free duration. Constant
// subtraction supports only a right-hand constant (Arith defines no
// duration-minus-temporal, and integer const-minus-prop is a different
// formula than the term encodes).
func i64TermOf(c cnode) (i64Term, bool) {
	switch n := c.(type) {
	case *cProp:
		if n.reader.node.kind != colI64 {
			return i64Term{}, false
		}
		return i64Term{slot: n.slot, col: n.reader.node.i64}, true
	case *cBin:
		if n.op != ast.OpAdd && n.op != ast.OpSub {
			return i64Term{}, false
		}
		p, lit, rev := splitPropLit(n.l, n.r)
		if p == nil || (rev && n.op == ast.OpSub) || p.reader.node.kind != colI64 {
			return i64Term{}, false
		}
		t := i64Term{slot: p.slot, col: p.reader.node.i64}
		if months, days, ms, isDur := lit.v.AsDuration(); isDur {
			if months != 0 {
				return i64Term{}, false
			}
			// Fold the months-free duration to a tick offset, checked: if the
			// constant itself overflows, decline the specialization so the
			// row falls to the tree, whose fixed ApplyDuration yields Null.
			tick, ok := mulChecked(days, eval.MSPerDay)
			if !ok {
				return i64Term{}, false
			}
			off, ok := addChecked(tick, ms)
			if !ok {
				return i64Term{}, false
			}
			if n.op == ast.OpSub {
				if off == math.MinInt64 {
					return i64Term{}, false
				}
				off = -off
			}
			t.mode, t.off = termDur, off
			return t, true
		}
		if k, isInt := lit.v.AsInt(); isInt {
			t.mode, t.off, t.sub = termInt, k, n.op == ast.OpSub
			return t, true
		}
	}
	return i64Term{}, false
}

// splitPropLit splits a binary's operands into (property, literal); rev
// marks the property on the right.
func splitPropLit(l, r cnode) (p *cProp, lit *cLit, rev bool) {
	if pp, ok := l.(*cProp); ok {
		if ll, ok2 := r.(*cLit); ok2 {
			return pp, ll, false
		}
	}
	if pp, ok := r.(*cProp); ok {
		if ll, ok2 := l.(*cLit); ok2 {
			return pp, ll, true
		}
	}
	return nil, nil, false
}

// slotVal mirrors cSlot evaluation: out-of-range reads are Null.
func slotVal(row []value.Value, s int) value.Value {
	if s >= 0 && s < len(row) {
		return row[s]
	}
	return value.Null()
}
