// The columnar-aggregate chain environment: resolving each per-row
// boundary projection against the scanned candidate, classifying an
// expression into a typed per-candidate key function (property read,
// temporal component, label test, searched CASE), and the range-window
// optimization that narrows the scan to an indexed key's window. Split
// from colagg.go, which holds the fusion driver (tryColumnarAggChain).
package exec

import (
	"math"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/compile"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// addCarried registers a carried (per-input-row constant) column value.
func (env *colEnv) addCarried(name string, v value.Value) {
	env.entries[name] = colEntry{carried: true, val: v}
	if _, ok := env.synthSlots[name]; !ok {
		env.synthSlots[name] = len(env.synthRow)
		env.synthRow = append(env.synthRow, v)
	} else {
		env.synthRow[env.synthSlots[name]] = v
	}
}

// applyBoundary re-maps the env through one pure per-row projection:
// every output column must resolve, or the chain declines.
func (env *colEnv) applyBoundary(proj *plan.ProjPlan) bool {
	next := map[string]colEntry{}
	for _, r := range proj.Returns {
		ent, ok := env.resolveExpr(r.Expr)
		if !ok {
			return false
		}
		next[r.Name] = ent
	}
	// Rebuild name spaces: keep synth slots for carried survivors and the
	// scanned var's aliases.
	entries := env.entries
	env.entries = next
	for nm, ent := range next {
		switch {
		case ent.isV:
			env.synthSlots[nm] = 0
		case ent.carried:
			if _, ok := env.synthSlots[nm]; !ok {
				env.synthSlots[nm] = len(env.synthRow)
				env.synthRow = append(env.synthRow, ent.val)
			} else {
				env.synthRow[env.synthSlots[nm]] = ent.val
			}
		}
	}
	_ = entries
	return true
}

// resolveExpr classifies one chain expression against the env: the
// scanned var itself, a carried value (evaluated now -- one input row),
// or a typed per-candidate key function.
func (env *colEnv) resolveExpr(e ast.Expr) (colEntry, bool) {
	if v, ok := e.(*ast.Var); ok {
		ent, ok := env.entries[v.Name]
		return ent, ok
	}
	if fn, kind, ok := env.classifyKeyFn(e); ok {
		return colEntry{fn: fn, kind: kind}, true
	}
	// No candidate dependency anywhere: a carried expression, evaluated
	// against the synthetic row.
	if env.mentionsCandidate(e) {
		return colEntry{}, false
	}
	c := compileEval(env.ctx, e, env.synthSlots)
	return colEntry{carried: true, val: c.Eval(env.ctx, env.synthRow, env.synthSlots)}, true
}

// mentionsCandidate reports whether e references the scanned var (under
// any alias) or a computed key column.
func (env *colEnv) mentionsCandidate(e ast.Expr) bool {
	for nm, ent := range env.entries {
		if (ent.isV || ent.fn != nil) && plan.MentionsVar(e, nm) {
			return true
		}
	}
	return false
}

// classifyKeyFn compiles an expression into a typed per-candidate key
// function: a computed column reference, an i64/bool property read, a
// temporal component over an i64 property, a single-label membership
// test, or a searched CASE whose conditions specialize to candidate
// predicates and whose results are integer literals.
func (env *colEnv) classifyKeyFn(e ast.Expr) (colKeyFn, colAggKeyKind, bool) {
	switch n := e.(type) {
	case *ast.Var:
		ent, ok := env.entries[n.Name]
		if !ok || ent.fn == nil {
			return nil, 0, false
		}
		return ent.fn, ent.kind, true
	case *ast.Prop:
		ent, ok := env.entries[n.Var]
		if !ok || !ent.isV {
			return nil, 0, false
		}
		col, ok := env.g.ColIndexed(n.Key)
		if !ok {
			return func(uint32) (int64, bool) { return 0, false }, cakInt, true
		}
		switch col.Dtype() {
		case chickpeas.DtypeI64:
			r := col.I64()
			return r.Get, cakInt, true
		case chickpeas.DtypeBool:
			r := col.Bool()
			return func(id uint32) (int64, bool) {
				b, ok := r.Get(id)
				if !ok {
					return 0, false
				}
				if b {
					return 1, true
				}
				return 0, true
			}, cakBool, true
		}
		return nil, 0, false
	case *ast.PropOf:
		base, ok := n.Base.(*ast.Prop)
		if !ok {
			return nil, 0, false
		}
		ent, ok := env.entries[base.Var]
		if !ok || !ent.isV {
			return nil, 0, false
		}
		col, ok := env.g.ColIndexed(base.Key)
		if !ok || col.Dtype() != chickpeas.DtypeI64 {
			return nil, 0, false
		}
		r := col.I64()
		comp := n.Key
		return func(id uint32) (int64, bool) {
			ms, ok := r.Get(id)
			if !ok {
				return 0, false
			}
			return eval.Component(ms, comp)
		}, cakInt, true
	case *ast.HasLabelExpr:
		ent, ok := env.entries[n.Var]
		if !ok || !ent.isV || n.Expr == nil || n.Expr.Kind != ast.LabelName {
			return nil, 0, false
		}
		set, has := env.g.NodesWithLabel(n.Expr.Name)
		dense := env.g.LabelDense(n.Expr.Name)
		return func(id uint32) (int64, bool) {
			in := false
			switch {
			case dense != nil:
				w := int(id) >> 6
				in = w < len(dense) && dense[w]>>(id&63)&1 == 1
			case has:
				in = set.Contains(id)
			}
			if in {
				return 1, true
			}
			return 0, true
		}, cakBool, true
	case *ast.Case:
		if n.Operand != nil {
			return nil, 0, false
		}
		type arm struct {
			pred compile.CandPred
			res  int64
		}
		arms := make([]arm, 0, len(n.Whens))
		never := func(int) bool { return false }
		isConst := func(s int) bool { return s != 0 && s < len(env.synthRow) }
		for _, w := range n.Whens {
			cc := compile.HoistCarriedIn(compile.HoistConstIn(env.ctx, compile.New(env.ctx, w.Cond, env.synthSlots, env.g), isConst, env.synthRow, env.synthSlots), never)
			p, ok := compile.CandidatePred(cc, 0, env.synthSlots)
			if !ok {
				return nil, 0, false
			}
			res, ok := intLit(w.Result)
			if !ok {
				return nil, 0, false
			}
			arms = append(arms, arm{pred: p, res: res})
		}
		var elseRes int64
		hasElse := false
		if n.Else != nil {
			var ok bool
			if elseRes, ok = intLit(n.Else); !ok {
				return nil, 0, false
			}
			hasElse = true
		}
		ctx, row := env.ctx, env.synthRow
		return func(id uint32) (int64, bool) {
			for _, a := range arms {
				if a.pred(ctx, row, graph.NodeID(id)) {
					return a.res, true
				}
			}
			if hasElse {
				return elseRes, true
			}
			return 0, false
		}, cakInt, true
	}
	return nil, 0, false
}

// colAggRangeWindow finds the tightest single-key range window implied by
// the scan WHERE's comparison conjuncts over the scanned var: every
// `v.key CMP const` with an ordered operator and an int/temporal constant
// narrows the window for its key, and the key with the smallest window
// wins. nil when no conjunct ranges over an indexed i64 column. The
// window is a superset of the filtered candidates (the predicates all
// re-run), so this only ever changes enumeration order and cost.
func colAggRangeWindow(env *colEnv, where ast.Expr, vName string, g *chickpeas.Snapshot) []uint32 {
	if where == nil {
		return nil
	}
	type bound struct {
		lo, hi         int64
		loIncl, hiIncl bool
	}
	bounds := map[string]*bound{}
	var conjs []ast.Expr
	plan.SplitAnd(where, &conjs)
	for _, c := range conjs {
		bin, ok := c.(*ast.Binary)
		if !ok {
			continue
		}
		p, isP := bin.LHS.(*ast.Prop)
		konst := bin.RHS
		op := bin.Op
		if !isP {
			if p, isP = bin.RHS.(*ast.Prop); !isP {
				continue
			}
			konst = bin.LHS
			op = flipCmp(op)
		}
		if p.Var != vName || env.mentionsCandidate(konst) {
			continue
		}
		cv := compileEval(env.ctx, konst, env.synthSlots).Eval(env.ctx, env.synthRow, env.synthSlots)
		var k int64
		if i, ok := cv.AsInt(); ok {
			k = i
		} else if ms, _, ok := cv.AsTemporal(); ok {
			k = ms
		} else {
			continue
		}
		b := bounds[p.Key]
		if b == nil {
			b = &bound{lo: math.MinInt64, hi: math.MaxInt64, loIncl: true, hiIncl: true}
			bounds[p.Key] = b
		}
		switch op {
		case ast.OpLt:
			if k <= b.hi {
				b.hi, b.hiIncl = k, false
			}
		case ast.OpLte:
			if k < b.hi {
				b.hi, b.hiIncl = k, true
			}
		case ast.OpGt:
			if k >= b.lo {
				b.lo, b.loIncl = k, false
			}
		case ast.OpGte:
			if k > b.lo {
				b.lo, b.loIncl = k, true
			}
		case ast.OpEq:
			if k > b.lo {
				b.lo, b.loIncl = k, true
			}
			if k < b.hi {
				b.hi, b.hiIncl = k, true
			}
		}
	}
	var best []uint32
	found := false
	for key, b := range bounds {
		if b.lo == math.MinInt64 && b.loIncl && b.hi == math.MaxInt64 && b.hiIncl {
			continue
		}
		ri, ok := g.ColRangeIndex(key)
		if !ok {
			continue
		}
		w := ri.Window(b.lo, b.hi, b.loIncl, b.hiIncl)
		if !found || len(w) < len(best) {
			best, found = w, true
		}
	}
	if !found {
		return nil
	}
	return best
}

// flipCmp mirrors a comparison across swapped operands.
func flipCmp(op ast.BinOp) ast.BinOp {
	switch op {
	case ast.OpLt:
		return ast.OpGt
	case ast.OpLte:
		return ast.OpGte
	case ast.OpGt:
		return ast.OpLt
	case ast.OpGte:
		return ast.OpLte
	}
	return op
}

// colAggPropValue is a boxed typed read of the scanned var's property
// column for aggregate arguments, absent folding to Null exactly like the
// compiled property reader.
func colAggPropValue(g *chickpeas.Snapshot, key string) (func(id uint32) value.Value, bool) {
	col, ok := g.ColIndexed(key)
	if !ok {
		return func(uint32) value.Value { return value.Null() }, true
	}
	switch col.Dtype() {
	case chickpeas.DtypeI64:
		r := col.I64()
		return func(id uint32) value.Value {
			if v, ok := r.Get(id); ok {
				return value.Int(v)
			}
			return value.Null()
		}, true
	case chickpeas.DtypeF64:
		r := col.F64()
		return func(id uint32) value.Value {
			if v, ok := r.Get(id); ok {
				return value.Float(v)
			}
			return value.Null()
		}, true
	}
	return nil, false
}

// intLit unwraps an integer literal expression.
func intLit(e ast.Expr) (int64, bool) {
	l, ok := e.(*ast.Lit)
	if !ok || l.Value.Kind != ast.LitInt {
		return 0, false
	}
	return l.Value.I, true
}
