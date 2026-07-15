// Per-row evaluation of a compiled expression, mirroring the interpreter
// exactly (the shared eval kernels keep the arithmetic, comparison, and
// function semantics identical).
package compile

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

func ceval(ctx *eval.Ctx, c cnode, g *chickpeas.Snapshot, row []value.Value, slots map[string]int) value.Value {
	switch n := c.(type) {
	case *cLit:
		return n.v
	case *cSlot:
		if n.s >= 0 && n.s < len(row) {
			return row[n.s]
		}
		return value.Null()
	case *cProp:
		return cevalProp(n, row)
	case *cCmpPropConst:
		l, r := cevalProp(n.prop, row), n.c
		if n.rev {
			l, r = r, l
		}
		o, ok := value.Compare(l, r)
		if !ok {
			return value.Null()
		}
		var b bool
		switch n.op {
		case ast.OpEq:
			b = o == 0
		case ast.OpNeq:
			b = o != 0
		case ast.OpLt:
			b = o < 0
		case ast.OpLte:
			b = o <= 0
		case ast.OpGt:
			b = o > 0
		default: // ast.OpGte
			b = o >= 0
		}
		return value.Bool(b)
	case *cNot:
		if b, ok := ceval(ctx, n.e, g, row, slots).AsBool(); ok {
			return value.Bool(!b)
		}
		return value.Null()
	case *cNeg:
		v := ceval(ctx, n.e, g, row, slots)
		if i, ok := v.AsInt(); ok {
			if i == -9223372036854775808 {
				return value.Null()
			}
			return value.Int(-i)
		}
		if v.Kind() == value.KindFloat {
			f, _ := v.AsFloat()
			return value.Float(-f)
		}
		return value.Null()
	case *cBin:
		return cevalBin(ctx, n, g, row, slots)
	case *cList:
		out := make([]value.Value, len(n.xs))
		for i, x := range n.xs {
			out[i] = ceval(ctx, x, g, row, slots)
		}
		return value.List(out)
	case *cIn:
		v := ceval(ctx, n.e, g, row, slots)
		if v.IsNull() {
			return value.Null()
		}
		xs, ok := ceval(ctx, n.list, g, row, slots).AsList()
		if !ok {
			return value.Null()
		}
		return inResult(xs, v)
	case *cInConst:
		v := ceval(ctx, n.e, g, row, slots)
		if v.IsNull() {
			return value.Null()
		}
		return n.m.resultFor(v)
	case *cInCarried:
		n.refresh(ctx, g, row, slots)
		v := ceval(ctx, n.e, g, row, slots)
		if v.IsNull() || n.notList {
			return value.Null()
		}
		return n.m.resultFor(v)
	case *cIsNull:
		return value.Bool(ceval(ctx, n.e, g, row, slots).IsNull() != n.negated)
	case *cSubquery:
		if n.decorOK && !n.decorOff {
			if c, ok := decorCount(ctx, n, row, slots); ok {
				return value.Int(int64(c))
			}
		}
		var count int
		switch {
		case n.hasMemo:
			if ik, ok := packNodeKey(n.memoSlots, row); ok {
				if c, hit := n.memoI[ik]; hit {
					count = c
				} else {
					count = eval.SubqueryCount(ctx, n.pattern, n.where, row, slots, !n.isCount)
					if n.memoI == nil {
						n.memoI = map[uint64]int{}
					}
					n.memoI[ik] = count
				}
				break
			}
			n.key = n.key[:0]
			for _, s := range n.memoSlots {
				n.key = value.AppendKey(n.key, row[s])
			}
			if c, hit := n.memo[string(n.key)]; hit {
				count = c
			} else {
				count = eval.SubqueryCount(ctx, n.pattern, n.where, row, slots, !n.isCount)
				n.memo[string(n.key)] = count
			}
		default:
			count = eval.SubqueryCount(ctx, n.pattern, n.where, row, slots, !n.isCount)
		}
		if n.isCount {
			return value.Int(int64(count))
		}
		return value.Bool(count > 0)
	case *cCase:
		if n.operand != nil {
			target := ceval(ctx, n.operand, g, row, slots)
			for _, w := range n.whens {
				if value.Equal(ceval(ctx, w[0], g, row, slots), target) {
					return ceval(ctx, w[1], g, row, slots)
				}
			}
		} else {
			for _, w := range n.whens {
				if ceval(ctx, w[0], g, row, slots).IsTruthy() {
					return ceval(ctx, w[1], g, row, slots)
				}
			}
		}
		if n.els != nil {
			return ceval(ctx, n.els, g, row, slots)
		}
		return value.Null()
	case *cFunc:
		argv := make([]value.Value, len(n.args))
		for i, a := range n.args {
			argv[i] = ceval(ctx, a, g, row, slots)
		}
		return eval.ApplyFunc(n.op, argv)
	case *cSlow:
		return eval.Eval(ctx, n.e, row, slots)
	}
	return value.Null()
}

// refresh re-evaluates a carried IN's list once per match-call epoch,
// rebuilding the membership index only when the list is a different value
// than last epoch's (a segment-stable slot skips every rebuild). Shared
// by the tree evaluation and the per-candidate specialized form.
func (n *cInCarried) refresh(ctx *eval.Ctx, g *chickpeas.Snapshot, row []value.Value, slots map[string]int) {
	if n.built && n.epoch == ctx.MatchEpoch {
		return
	}
	lv := ceval(ctx, n.list, g, row, slots)
	if xs, ok := lv.AsList(); ok {
		if !n.built || n.notList || !value.SameBacking(lv, n.lastList) {
			n.m = buildMembership(xs)
			n.lastList = lv
		}
		n.notList = false
	} else {
		n.notList = true
	}
	n.built = true
	n.epoch = ctx.MatchEpoch
}

// packNodeKey packs the correlated slots of a subquery into a single
// uint64 memo key when they are one or two node ids, ok=false otherwise
// (falling the caller back to the byte-string memo). Two node slots pack as
// id0<<32 | id1; node ids fit u32, matching AppendKey's tagNode encoding.
// Restricting the fast path to node kinds keeps a node and a relationship
// of equal raw id from conflating, since a fixed subquery's memoSlots hold
// a stable kind per position.
func packNodeKey(slots []int, row []value.Value) (uint64, bool) {
	switch len(slots) {
	case 1:
		if id, ok := row[slots[0]].AsNode(); ok {
			return uint64(uint32(id)), true
		}
	case 2:
		id0, ok0 := row[slots[0]].AsNode()
		id1, ok1 := row[slots[1]].AsNode()
		if ok0 && ok1 {
			return uint64(uint32(id0))<<32 | uint64(uint32(id1)), true
		}
	}
	return 0, false
}

// decorFanoutCap bounds the anchor-selection degree probe: distinguishing a
// hub endpoint from a leaf needs only enough of a count to compare, and the
// probe runs once, on the first non-null row of a hot query.
const decorFanoutCap = 1 << 16

// astDirToGraph maps a pattern direction to the graph traversal direction.
func astDirToGraph(d ast.Dir) chickpeas.Direction {
	switch d {
	case ast.DirOut:
		return chickpeas.Outgoing
	case ast.DirIn:
		return chickpeas.Incoming
	}
	return chickpeas.Both
}

// decorFanout counts node's typed neighbors in dir, capped: a hub only needs
// to out-count a leaf, not be measured exactly.
func decorFanout(ctx *eval.Ctx, node chickpeas.NodeID, dir chickpeas.Direction, types []string) int {
	n := 0
	for range ctx.G.NeighborsByType(node, dir, types) {
		if n++; n >= decorFanoutCap {
			break
		}
	}
	return n
}

// decorCount answers a decorrelatable COUNT{} from a per-anchor side table
// (task 084): the subquery is evaluated ONCE per distinct anchor node (grouped
// by the other endpoint) and each row is an O(1) map read. The anchor endpoint
// -- the hubbier end by typed first-hop degree -- is chosen once and reused;
// correctness is anchor-independent, so only the amortization depends on it.
// ok=false hands the row back to the per-row memo/DFS path: a null endpoint,
// or too many distinct anchors (the anchor is not amortizing, so decor is
// switched off for the rest of the run).
func decorCount(ctx *eval.Ctx, n *cSubquery, row []value.Value, slots map[string]int) (int, bool) {
	startNode, ok1 := row[n.decorStartSlot].AsNode()
	endNode, ok2 := row[n.decorEndSlot].AsNode()
	if !ok1 || !ok2 {
		return 0, false
	}
	hops := n.pattern.Hops
	if !n.decorAnchorDecided {
		startDeg := decorFanout(ctx, startNode, astDirToGraph(hops[0].Rel.Dir), hops[0].Rel.Types)
		last := hops[len(hops)-1]
		endDeg := decorFanout(ctx, endNode, astDirToGraph(last.Rel.Dir).Reverse(), last.Rel.Types)
		n.decorAnchorIsEnd = endDeg > startDeg
		n.decorAnchorDecided = true
	}
	anchorVar, groupVar := n.decorStartVar, n.decorEndVar
	anchorNode, groupNode := startNode, endNode
	if n.decorAnchorIsEnd {
		anchorVar, groupVar = n.decorEndVar, n.decorStartVar
		anchorNode, groupNode = endNode, startNode
	}
	// Tables live on the Ctx keyed by (canonical identity, anchor node), so
	// sibling subqueries that differ only in their outer endpoint names
	// share one table per anchor. The canonical identity embeds the anchor
	// ROLE (the substitution marker sits where the anchor sits in the
	// pattern), so siblings whose runtime anchor choice diverged simply
	// stop sharing rather than mix tables. A shape with no canonical
	// identity keys privately by node pointer -- same store, no sharing.
	if !n.decorCanonDone {
		n.decorCanon = decorCanon(n.pattern, n.where, anchorVar, groupVar)
		if n.decorCanon == "" {
			n.decorCanon = fmt.Sprintf("\x00%p", n)
		}
		n.decorCanonDone = true
	}
	// The one-entry cache answers the constant-anchor common case with an
	// integer compare; only an anchor CHANGE consults the shared store
	// (whose key hashes the canonical identity string).
	if n.decorLastTbl != nil && n.decorLastAnchor == uint32(anchorNode) {
		return n.decorLastTbl[groupNode], true
	}
	key := eval.DecorTableKey{Canon: n.decorCanon, Anchor: uint32(anchorNode)}
	n.decorProbes++
	tbl, hit := ctx.DecorTables[key]
	if !hit {
		if n.decorBuilds >= decorAnchorCap {
			n.decorOff = true
			return 0, false
		}
		tbl = eval.SubqueryGroupCount(ctx, n.pattern, n.where, row, slots, anchorVar, groupVar)
		if ctx.DecorTables == nil {
			ctx.DecorTables = map[eval.DecorTableKey]map[chickpeas.NodeID]int{}
		}
		ctx.DecorTables[key] = tbl
		n.decorBuilds++
		ctx.DecorBuilds++
	}
	n.decorLastAnchor, n.decorLastTbl = uint32(anchorNode), tbl
	return tbl[groupNode], true
}

// cevalBin mirrors the interpreter's binary dispatch over compiled
// operands.
// cevalProp reads a compiled property access against the row: a graph
// column for node/rel slot values, a map field, or a temporal component
// of an epoch-millis int; anything else is Null.
func cevalProp(n *cProp, row []value.Value) value.Value {
	if n.slot < 0 || n.slot >= len(row) {
		return value.Null()
	}
	base := row[n.slot]
	switch base.Kind() {
	case value.KindNode:
		id, _ := base.AsNode()
		return n.reader.readNode(id)
	case value.KindRel:
		pos, _ := base.AsRel()
		return n.reader.readRel(pos)
	case value.KindMap:
		entries, _ := base.AsMap()
		for _, e := range entries {
			if e.Key == n.reader.key {
				return e.Val
			}
		}
		return value.Null()
	case value.KindInt:
		// Temporal accessor over an i64 epoch-millis value.
		ms, _ := base.AsInt()
		if comp, ok := eval.Component(ms, n.reader.key); ok {
			return value.Int(comp)
		}
		return value.Null()
	case value.KindTemporal:
		ms, _, _ := base.AsTemporal()
		if comp, ok := eval.Component(ms, n.reader.key); ok {
			return value.Int(comp)
		}
		return value.Null()
	}
	return value.Null()
}

func cevalBin(ctx *eval.Ctx, n *cBin, g *chickpeas.Snapshot, row []value.Value, slots map[string]int) value.Value {
	switch n.op {
	case ast.OpAnd:
		l, lk := value.ThreeValued(ceval(ctx, n.l, g, row, slots))
		if lk && !l {
			return value.Bool(false)
		}
		r, rk := value.ThreeValued(ceval(ctx, n.r, g, row, slots))
		return value.KleeneAnd(l, lk, r, rk)
	case ast.OpOr:
		l, lk := value.ThreeValued(ceval(ctx, n.l, g, row, slots))
		if lk && l {
			return value.Bool(true)
		}
		r, rk := value.ThreeValued(ceval(ctx, n.r, g, row, slots))
		return value.KleeneOr(l, lk, r, rk)
	}
	l := ceval(ctx, n.l, g, row, slots)
	r := ceval(ctx, n.r, g, row, slots)
	switch n.op {
	case ast.OpEq:
		return eval.CmpBool(l, r, func(o int) bool { return o == 0 })
	case ast.OpNeq:
		return eval.CmpBool(l, r, func(o int) bool { return o != 0 })
	case ast.OpLt:
		return eval.CmpBool(l, r, func(o int) bool { return o < 0 })
	case ast.OpLte:
		return eval.CmpBool(l, r, func(o int) bool { return o <= 0 })
	case ast.OpGt:
		return eval.CmpBool(l, r, func(o int) bool { return o > 0 })
	case ast.OpGte:
		return eval.CmpBool(l, r, func(o int) bool { return o >= 0 })
	case ast.OpStartsWith, ast.OpEndsWith, ast.OpContains:
		return eval.StrPred(n.op, l, r)
	default:
		return eval.Arith(n.op, l, r)
	}
}

// inResult is the openCypher IN result over a materialized list: true on
// a hit, else null when the list contains a null element, else false.
func inResult(xs []value.Value, v value.Value) value.Value {
	sawNull := false
	for _, x := range xs {
		if value.Equal(x, v) {
			return value.Bool(true)
		}
		if x.IsNull() {
			sawNull = true
		}
	}
	if sawNull {
		return value.Null()
	}
	return value.Bool(false)
}
