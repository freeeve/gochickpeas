// Fused columnar aggregation: a chain marked by plan/colagg.go -- one
// bare label-scan segment, a run of stage-less LET boundaries, and a
// stage-less aggregated boundary -- runs as ONE typed pass over the
// scanned label's ids. Filters run through the per-candidate predicate
// machinery, group keys and aggregate arguments through typed column
// reads resolved across the boundary chain by name; grouping, aggregate
// state, post-aggregation wrappers, ordering, and pagination reuse the
// general path's exact kernels (aggState, sortRowsByOrder, paginate), so
// the fused result is identical by construction. ANY expression the
// classifier cannot express over typed columns declines the whole chain
// back to the general pipeline. Classification is shape-generic: it
// never asks what query a chain came from, only whether its expressions
// fit the typed vocabulary.
package exec

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/compile"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// disableColAgg pins differential tests to the general path.
var disableColAgg = false

// colAggMaxKeys is the fused group key width (a fixed-size comparable map
// key); wider groupings keep the general path.
const colAggMaxKeys = 4

// colAggKeyKind says how a computed key's int64 unboxes for output.
type colAggKeyKind uint8

const (
	cakInt colAggKeyKind = iota
	cakBool
)

// colKeyFn is a typed per-candidate key: ok=false is a null key (its own
// group), mirroring the boxed evaluation's Null.
type colKeyFn func(id uint32) (int64, bool)

// colEntry is one resolved column name flowing through the chain.
type colEntry struct {
	isV     bool
	fn      colKeyFn
	kind    colAggKeyKind
	carried bool
	val     value.Value // carried entries: the (single input row) value
}

// colEnv resolves column names during the chain walk. synthSlots/synthRow
// let chain expressions compile against carried values and the scanned
// candidate (slot 0) through the ordinary compile machinery.
type colEnv struct {
	entries    map[string]colEntry
	synthSlots map[string]int
	synthRow   []value.Value
	ctx        *eval.Ctx
	g          *chickpeas.Snapshot
}

// tryColumnarAggChain fuses the chain headed at segments[i], reporting
// (output rows, segments consumed, true) or ok=false with nothing
// consumed. Exactness domain: exactly one input row (with more, carried
// group keys would participate in cross-row grouping).
func tryColumnarAggChain(ctx *eval.Ctx, segments []*plan.Segment, i int, inputs [][]value.Value) ([][]value.Value, int, bool) {
	if disableColAgg || len(inputs) != 1 {
		return nil, 0, false
	}
	n := plan.ColAggChainLen(segments, i)
	if n == 0 {
		return nil, 0, false
	}
	native, ok := ctx.G.(graph.Native)
	if !ok {
		return nil, 0, false
	}
	g := native.Snapshot()
	head := segments[i]
	ms := head.Stages[0].(*plan.MatchStage)
	op := &ms.Ops[0]
	vSlot := op.Slot
	vName := ""
	for nm, s := range head.Slots {
		if s == vSlot {
			vName = nm
		}
	}
	if vName == "" {
		return nil, 0, false
	}

	// Head-segment scratch row: the seeded input, for carried references
	// in filters and carried column evaluation.
	scratch := make([]value.Value, head.RowWidth)
	copy(scratch, inputs[0])
	isConst := func(s int) bool { return s != vSlot && s >= 0 && s < len(scratch) }
	never := func(int) bool { return false }
	ctx.MatchEpoch++

	// Filters: every WHERE conjunct of the scan stage must specialize to
	// a per-candidate predicate.
	var preds []compile.CandPred
	if ms.Where != nil {
		var conjs []ast.Expr
		plan.SplitAnd(ms.Where, &conjs)
		for _, c := range conjs {
			cc := compile.HoistCarriedIn(compile.HoistConstIn(ctx, compile.New(ctx, c, head.Slots, g), isConst, scratch, head.Slots), never)
			p, ok := compile.CandidatePred(cc, vSlot, head.Slots)
			if !ok {
				return nil, 0, false
			}
			preds = append(preds, p)
		}
	}

	// The chain env starts from the head's input columns (carried) and
	// the scanned var, then each boundary re-maps it through its Returns.
	env := &colEnv{entries: map[string]colEntry{}, synthSlots: map[string]int{}, ctx: ctx, g: g}
	env.synthSlots[vName] = 0
	env.synthRow = append(env.synthRow, value.Value{})
	env.entries[vName] = colEntry{isV: true}
	for nm, s := range head.Slots {
		if nm == vName {
			continue
		}
		var v value.Value
		if s >= 0 && s < len(inputs[0]) {
			v = inputs[0][s]
		}
		env.addCarried(nm, v)
	}
	// Boundaries: the head's own projection, then each pass-through
	// segment's, resolving every output column.
	for j := i; j < i+n-1 || (n == 1 && j == i); j++ {
		if !env.applyBoundary(&segments[j].Proj) {
			return nil, 0, false
		}
		if n == 1 {
			break
		}
	}
	last := segments[i+n-1]
	proj := &last.Proj
	if n > 1 {
		// Interior boundaries were applied above (head..last-1); the
		// aggregated boundary itself is consumed below, not re-mapped.
		_ = proj
	}

	// Group keys and aggregate arguments resolve against the final env.
	var keys []colEntry
	computed := 0
	for _, gi := range proj.GroupIdx {
		e, ok := env.resolveExpr(proj.Returns[gi].Expr)
		if !ok {
			return nil, 0, false
		}
		if e.isV {
			return nil, 0, false
		}
		keys = append(keys, e)
		if !e.carried {
			computed++
		}
	}
	if computed > colAggMaxKeys {
		return nil, 0, false
	}
	var args []struct {
		read func(id uint32) value.Value
		star bool
	}
	for _, a := range proj.Aggs {
		switch arg := a.Arg.(type) {
		case nil:
			args = append(args, struct {
				read func(id uint32) value.Value
				star bool
			}{star: true})
		case *ast.Var:
			ent, ok := env.entries[arg.Name]
			if !ok || !ent.isV {
				return nil, 0, false
			}
			args = append(args, struct {
				read func(id uint32) value.Value
				star bool
			}{read: func(id uint32) value.Value { return value.Node(graph.NodeID(id)) }})
		case *ast.Prop:
			ent, ok := env.entries[arg.Var]
			if !ok || !ent.isV {
				return nil, 0, false
			}
			read, ok := colAggPropValue(g, arg.Key)
			if !ok {
				return nil, 0, false
			}
			args = append(args, struct {
				read func(id uint32) value.Value
				star bool
			}{read: read})
		default:
			return nil, 0, false
		}
	}

	// The fused pass: filter, key, accumulate.
	ids, hasLabel := g.NodesWithLabel(op.Source.Label)
	type groupKey struct {
		v    [colAggMaxKeys]int64
		null uint8
	}
	index := map[groupKey]int{}
	var states []aggState
	var groupKeys []groupKey
	scan := func(id uint32) {
		for _, p := range preds {
			if !p(ctx, scratch, graph.NodeID(id)) {
				return
			}
		}
		var gk groupKey
		ki := 0
		for _, k := range keys {
			if k.carried {
				continue
			}
			v, ok := k.fn(id)
			if ok {
				gk.v[ki] = v
			} else {
				gk.null |= 1 << ki
			}
			ki++
		}
		idx, hit := index[gk]
		if !hit {
			idx = len(groupKeys)
			index[gk] = idx
			groupKeys = append(groupKeys, gk)
			for _, a := range proj.Aggs {
				states = append(states, aggState{kind: a.Kind})
			}
		}
		st := states[idx*len(proj.Aggs) : (idx+1)*len(proj.Aggs)]
		for j, a := range args {
			if a.star {
				st[j].update(value.Value{}, false)
			} else {
				st[j].update(a.read(id), true)
			}
		}
	}
	if hasLabel {
		for id := range ids.Iter() {
			scan(id)
		}
	}

	// A keyless aggregate over no input still emits one zeroed group,
	// exactly like the general accumulator.
	if len(groupKeys) == 0 && len(proj.GroupIdx) == 0 {
		groupKeys = append(groupKeys, groupKey{})
		for _, a := range proj.Aggs {
			states = append(states, aggState{kind: a.Kind})
		}
	}

	// Output assembly mirrors aggregator.finalize: keys and aggregate
	// finals into a hidden-slot-extended row, post wrappers, truncate,
	// order, paginate.
	nCols := len(proj.Returns)
	postSlots := make(map[string]int, proj.NHidden+len(proj.GroupIdx))
	for k := 0; k < proj.NHidden; k++ {
		postSlots[hiddenAggName(k)] = nCols + k
	}
	for _, gi := range proj.GroupIdx {
		if _, ok := postSlots[proj.Returns[gi].Name]; !ok {
			postSlots[proj.Returns[gi].Name] = gi
		}
	}
	postC := make([]RowEval, len(proj.Post))
	for pi, p := range proj.Post {
		postC[pi] = compileEval(ctx, p.Expr, postSlots)
	}
	out := make([][]value.Value, 0, len(groupKeys))
	for idx, gk := range groupKeys {
		row := make([]value.Value, nCols+proj.NHidden)
		ki := 0
		for k, gi := range proj.GroupIdx {
			ent := keys[k]
			if ent.carried {
				row[gi] = ent.val
				continue
			}
			switch {
			case gk.null&(1<<ki) != 0:
				row[gi] = value.Null()
			case ent.kind == cakBool:
				row[gi] = value.Bool(gk.v[ki] != 0)
			default:
				row[gi] = value.Int(gk.v[ki])
			}
			ki++
		}
		st := states[idx*len(proj.Aggs) : (idx+1)*len(proj.Aggs)]
		for j := range proj.Aggs {
			row[proj.Aggs[j].OutIdx] = st[j].finalize()
		}
		for pi, p := range proj.Post {
			row[p.Col] = postC[pi].Eval(ctx, row, postSlots)
		}
		out = append(out, row[:nCols])
	}
	if len(proj.OrderBy) > 0 {
		out = sortRowsByOrder(ctx, proj, last.Slots, func(int) []value.Value { return nil }, 0, out)
	}
	out = paginate(out, proj.Skip, proj.Limit)
	if last.PostWhere != nil {
		applyPostWhere(ctx, last, &out)
	}
	return out, n, true
}

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
