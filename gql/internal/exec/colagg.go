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

// disableColAgg pins differential tests to the general path;
// colAggFired counts successful fusions so tests can assert the fused
// arm actually took the fused path (a silent double-general run would
// pass any differential vacuously).
var (
	disableColAgg = false
	colAggFired   int
)

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
	// Boundaries: the head's projection and each interior pass-through
	// re-map the env; the aggregated boundary itself is consumed by the
	// key/argument classification below, never re-mapped (its returns
	// are the aggregate outputs). A single-segment chain applies no
	// boundary at all -- its keys and arguments resolve over the head's
	// own inputs.
	for j := i; j < i+n-1; j++ {
		if !env.applyBoundary(&segments[j].Proj) {
			return nil, 0, false
		}
	}
	last := segments[i+n-1]
	proj := &last.Proj

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
		// A selective range conjunct flips the enumeration: instead of
		// every labeled node testing every predicate, walk the range
		// index's window (exact count, no estimate) and test label
		// membership -- chosen only when the window is decisively smaller
		// than the label, so broad ranges keep the cache-linear label
		// sweep. Every predicate still runs per candidate (the window is
		// a superset reduction), so results are identical either way.
		if win := colAggRangeWindow(env, ms.Where, vName, g); win != nil && len(win) < ids.Len()/4 {
			dense := g.LabelDense(op.Source.Label)
			for _, id := range win {
				in := false
				if dense != nil {
					w := int(id) >> 6
					in = w < len(dense) && dense[w]>>(id&63)&1 == 1
				} else {
					in = ids.Contains(id)
				}
				if in {
					scan(id)
				}
			}
		} else {
			for id := range ids.Iter() {
				scan(id)
			}
		}
	}

	// No candidate survived. A keyless aggregate over no input still
	// emits one zeroed group, exactly like the general accumulator. An
	// OPTIONAL scan instead emits its null-fill row THROUGH the
	// aggregator: star aggregates count that row (count(*) reads 1),
	// value aggregates read null from it (their identity), computed
	// group keys take the null key, carried keys keep their carried
	// values. Established against the general path's own output --
	// grouped plain MATCH stays zero rows, everything else fills.
	if len(groupKeys) == 0 && (len(proj.GroupIdx) == 0 || ms.Optional) {
		gk := groupKey{}
		if ms.Optional {
			for ki := 0; ki < computed; ki++ {
				gk.null |= 1 << ki
			}
		}
		groupKeys = append(groupKeys, gk)
		for _, a := range proj.Aggs {
			states = append(states, aggState{kind: a.Kind})
		}
		if ms.Optional {
			for j, a := range args {
				if a.star {
					states[j].update(value.Value{}, false)
				} else {
					states[j].update(value.Null(), true)
				}
			}
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
	colAggFired++
	return out, n, true
}
