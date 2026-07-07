// CALL-procedure planning (port of the Rust plan/call.rs, grown for
// expression arguments): resolve the procedure kind by name, fold constant
// arguments and validate them at bind time, and map the YIELD fields to
// output slots. A call with any non-constant argument is correlated: its
// arguments are checked against the in-scope variables here and evaluated
// per input row at exec, where a row whose arguments fail validation
// yields no rows (total-eval semantics; static mistakes still error here).
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

func buildCallStage(proc string, args []ast.Expr, yields []ast.YieldItem, slots map[string]int, bound map[int]bool, nextSlot *int) (*CallStage, error) {
	kind, ok := procKindOf(proc)
	if !ok {
		return nil, planErrf("unknown procedure `%s` (supported: wcc, algo.bfs, algo.pagerank, algo.wcc, algo.cdlp, algo.lcc, algo.sssp, algo.propagate, fts.search, geo.withinRadius, geo.withinBBox)", proc)
	}
	cs := &CallStage{}
	vals, static := constArgs(args)
	if static {
		p, err := ResolveCallProc(proc, vals)
		if err != nil {
			return nil, err
		}
		cs.Proc = p
	} else {
		for _, a := range args {
			if semantics.ExprHasAgg(a) {
				return nil, planErrf("aggregates are not allowed in procedure arguments")
			}
			if err := semantics.CheckRefs(a, slots); err != nil {
				return nil, err
			}
		}
		cs.Proc = CallProc{Kind: kind}
		cs.ProcName = proc
		cs.ArgExprs = args
	}
	valueField := valueFieldName(kind)

	nodeSlot, valueSlot, depthSlot := NoSlot, NoSlot, NoSlot
	for _, y := range yields {
		name := y.Alias
		if name == "" {
			name = y.Field
		}
		slot := *nextSlot
		*nextSlot++
		slots[name] = slot
		bound[slot] = true
		switch {
		case y.Field == "node":
			nodeSlot = slot
		case valueField != "" && y.Field == valueField:
			valueSlot = slot
		case kind == ProcPropagate && y.Field == "depth":
			depthSlot = slot
		default:
			return nil, planErrf("procedure `%s` does not yield `%s`", proc, y.Field)
		}
	}
	cs.NodeSlot, cs.ValueSlot, cs.DepthSlot = nodeSlot, valueSlot, depthSlot
	return cs, nil
}

// procKindOf resolves a case-insensitive procedure name to its kind.
func procKindOf(proc string) (ProcKind, bool) {
	switch lower(proc) {
	case "wcc":
		return ProcWcc, true
	case "fts.search":
		return ProcFtsSearch, true
	case "geo.withinradius":
		return ProcGeoWithinRadius, true
	case "geo.withinbbox":
		return ProcGeoWithinBBox, true
	case "algo.bfs":
		return ProcBfs, true
	case "algo.pagerank":
		return ProcPageRank, true
	case "algo.wcc":
		return ProcWccAll, true
	case "algo.cdlp":
		return ProcCdlp, true
	case "algo.lcc":
		return ProcLcc, true
	case "algo.sssp":
		return ProcSssp, true
	case "algo.propagate":
		return ProcPropagate, true
	}
	return 0, false
}

// valueFieldName is the per-node scalar column a procedure yields
// alongside node ("" for the search procedures, which yield only node).
func valueFieldName(kind ProcKind) string {
	switch kind {
	case ProcWcc:
		return "component"
	case ProcBfs, ProcPageRank, ProcWccAll, ProcCdlp, ProcLcc, ProcSssp, ProcPropagate:
		return "value"
	}
	return ""
}

// constArgs folds every argument to a constant value; ok is false when any
// argument needs row context (making the call correlated).
func constArgs(args []ast.Expr) ([]value.Value, bool) {
	vals := make([]value.Value, len(args))
	for i, a := range args {
		v, ok := constArg(a)
		if !ok {
			return nil, false
		}
		vals[i] = v
	}
	return vals, true
}

// constArg folds one constant argument: a literal, a negated numeric
// literal, or a list of constants. Parameters are not constants (they
// resolve per run through the correlated path).
func constArg(e ast.Expr) (value.Value, bool) {
	switch n := e.(type) {
	case *ast.Lit:
		if n.Value.Kind == ast.LitParam || n.Value.Kind == ast.LitNamedParam {
			return value.Value{}, false
		}
		return semantics.LitValue(n.Value), true
	case *ast.Unary:
		if n.Op != ast.Neg {
			return value.Value{}, false
		}
		v, ok := constArg(n.Expr)
		if !ok {
			return value.Value{}, false
		}
		if i, isInt := v.AsInt(); isInt {
			return value.Int(-i), true
		}
		if f, isFloat := v.AsFloat(); isFloat {
			return value.Float(-f), true
		}
	case *ast.ListExpr:
		elems := make([]value.Value, len(n.Elems))
		for i, el := range n.Elems {
			v, ok := constArg(el)
			if !ok {
				return value.Value{}, false
			}
			elems[i] = v
		}
		return value.List(elems), true
	}
	return value.Value{}, false
}

// ResolveCallProc validates resolved argument values into a concrete
// CallProc -- at bind time for constant arguments, per input row from exec
// for correlated ones.
func ResolveCallProc(proc string, args []value.Value) (CallProc, error) {
	switch lower(proc) {
	case "wcc":
		relType, err := strArg(proc, args, 0, "relType")
		if err != nil {
			return CallProc{}, err
		}
		dir := graph.Both
		if len(args) > 1 {
			if dir, err = dirArg(proc, args, 1); err != nil {
				return CallProc{}, err
			}
		}
		return CallProc{Kind: ProcWcc, RelType: relType, Direction: dir}, nil
	case "fts.search":
		if err := expectArgCount(proc, args, 3, "label, field, searchTerm"); err != nil {
			return CallProc{}, err
		}
		label, err := strArg(proc, args, 0, "label")
		if err != nil {
			return CallProc{}, err
		}
		field, err := strArg(proc, args, 1, "field")
		if err != nil {
			return CallProc{}, err
		}
		query, err := strArg(proc, args, 2, "searchTerm")
		if err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcFtsSearch, Label: label, Field: field, Query: query}, nil
	case "geo.withinradius":
		if err := expectArgCount(proc, args, 6, "label, latField, longField, lat, lon, km"); err != nil {
			return CallProc{}, err
		}
		out := CallProc{Kind: ProcGeoWithinRadius}
		var err error
		if out.Label, err = strArg(proc, args, 0, "label"); err != nil {
			return CallProc{}, err
		}
		if out.LatField, err = strArg(proc, args, 1, "latField"); err != nil {
			return CallProc{}, err
		}
		if out.LonField, err = strArg(proc, args, 2, "longField"); err != nil {
			return CallProc{}, err
		}
		if out.Lat, err = f64Arg(proc, args, 3, "lat"); err != nil {
			return CallProc{}, err
		}
		if out.Lon, err = f64Arg(proc, args, 4, "lon"); err != nil {
			return CallProc{}, err
		}
		if out.Km, err = f64Arg(proc, args, 5, "km"); err != nil {
			return CallProc{}, err
		}
		return out, nil
	case "geo.withinbbox":
		if err := expectArgCount(proc, args, 7, "label, latField, longField, minLat, minLon, maxLat, maxLon"); err != nil {
			return CallProc{}, err
		}
		out := CallProc{Kind: ProcGeoWithinBBox}
		var err error
		if out.Label, err = strArg(proc, args, 0, "label"); err != nil {
			return CallProc{}, err
		}
		if out.LatField, err = strArg(proc, args, 1, "latField"); err != nil {
			return CallProc{}, err
		}
		if out.LonField, err = strArg(proc, args, 2, "longField"); err != nil {
			return CallProc{}, err
		}
		if out.MinLat, err = f64Arg(proc, args, 3, "minLat"); err != nil {
			return CallProc{}, err
		}
		if out.MinLon, err = f64Arg(proc, args, 4, "minLon"); err != nil {
			return CallProc{}, err
		}
		if out.MaxLat, err = f64Arg(proc, args, 5, "maxLat"); err != nil {
			return CallProc{}, err
		}
		if out.MaxLon, err = f64Arg(proc, args, 6, "maxLon"); err != nil {
			return CallProc{}, err
		}
		return out, nil
	case "algo.bfs":
		if err := maxArgs(proc, args, 2, "source[, directed]"); err != nil {
			return CallProc{}, err
		}
		src, err := nodeArg(proc, args, 0, "source")
		if err != nil {
			return CallProc{}, err
		}
		directed, err := boolArgOr(proc, args, 1, "directed", false)
		if err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcBfs, Source: src, Directed: directed}, nil
	case "algo.pagerank":
		if err := maxArgs(proc, args, 3, "[directed][, damping][, iterations]"); err != nil {
			return CallProc{}, err
		}
		directed, err := boolArgOr(proc, args, 0, "directed", false)
		if err != nil {
			return CallProc{}, err
		}
		damping, err := f64ArgOr(proc, args, 1, "damping", 0.85)
		if err != nil {
			return CallProc{}, err
		}
		iters, err := u32ArgOr(proc, args, 2, "iterations", 20)
		if err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcPageRank, Directed: directed, Damping: damping, Iters: iters}, nil
	case "algo.wcc":
		if err := maxArgs(proc, args, 0, ""); err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcWccAll}, nil
	case "algo.cdlp":
		if err := maxArgs(proc, args, 3, "[directed][, iterations][, seedProp]"); err != nil {
			return CallProc{}, err
		}
		directed, err := boolArgOr(proc, args, 0, "directed", false)
		if err != nil {
			return CallProc{}, err
		}
		iters, err := u32ArgOr(proc, args, 1, "iterations", 10)
		if err != nil {
			return CallProc{}, err
		}
		seed := ""
		if len(args) > 2 {
			if seed, err = strArg(proc, args, 2, "seedProp"); err != nil {
				return CallProc{}, err
			}
		}
		return CallProc{Kind: ProcCdlp, Directed: directed, Iters: iters, SeedProp: seed}, nil
	case "algo.lcc":
		if err := maxArgs(proc, args, 1, "[directed]"); err != nil {
			return CallProc{}, err
		}
		directed, err := boolArgOr(proc, args, 0, "directed", false)
		if err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcLcc, Directed: directed}, nil
	case "algo.sssp":
		if err := maxArgs(proc, args, 3, "source[, directed][, weighted]"); err != nil {
			return CallProc{}, err
		}
		src, err := nodeArg(proc, args, 0, "source")
		if err != nil {
			return CallProc{}, err
		}
		directed, err := boolArgOr(proc, args, 1, "directed", false)
		if err != nil {
			return CallProc{}, err
		}
		weighted, err := boolArgOr(proc, args, 2, "weighted", false)
		if err != nil {
			return CallProc{}, err
		}
		return CallProc{Kind: ProcSssp, Source: src, Directed: directed, Weighted: weighted}, nil
	case "algo.propagate":
		return resolvePropagate(proc, args)
	}
	return CallProc{}, planErrf("unknown procedure `%s` (supported: wcc, algo.bfs, algo.pagerank, algo.wcc, algo.cdlp, algo.lcc, algo.sssp, algo.propagate, fts.search, geo.withinRadius, geo.withinBBox)", proc)
}

// resolvePropagate validates algo.propagate(seeds, values, relTypes,
// direction, maxDepth, valueProp, order, truncLimit[, minValue[,
// filterProp, filterMin, filterMax]]) -- first-claim value propagation
// (Snapshot.PropagateBFS), yielding node, value, depth per reached node.
func resolvePropagate(proc string, args []value.Value) (CallProc, error) {
	const sig = "seeds, values, relTypes, direction, maxDepth, valueProp, order, truncLimit[, minValue[, filterProp, filterMin, filterMax]]"
	if len(args) < 8 {
		return CallProc{}, planErrf("%s(%s) expects at least 8 arguments, got %d", proc, sig, len(args))
	}
	if err := maxArgs(proc, args, 12, sig); err != nil {
		return CallProc{}, err
	}
	out := CallProc{Kind: ProcPropagate}
	var err error
	if out.Seeds, err = nodesArg(proc, args, 0, "seeds"); err != nil {
		return CallProc{}, err
	}
	if out.SeedVals, err = f64sArg(proc, args, 1, "values"); err != nil {
		return CallProc{}, err
	}
	if len(out.Seeds) != len(out.SeedVals) {
		return CallProc{}, planErrf("%s seeds and values must be the same length (got %d and %d)", proc, len(out.Seeds), len(out.SeedVals))
	}
	if out.RelTypes, err = strsArg(proc, args, 2, "relTypes"); err != nil {
		return CallProc{}, err
	}
	if out.Direction, err = dirArg(proc, args, 3); err != nil {
		return CallProc{}, err
	}
	depth, err := u32ArgOr(proc, args, 4, "maxDepth", 0)
	if err != nil {
		return CallProc{}, err
	}
	out.MaxDepth = depth
	if out.ValueProp, err = strArg(proc, args, 5, "valueProp"); err != nil {
		return CallProc{}, err
	}
	order, err := strArg(proc, args, 6, "order")
	if err != nil {
		return CallProc{}, err
	}
	switch lower(order) {
	case "asc":
	case "desc":
		out.Desc = true
	default:
		return CallProc{}, planErrf("%s order must be 'asc' or 'desc', got `%s`", proc, order)
	}
	trunc, err := u32ArgOr(proc, args, 7, "truncLimit", 0)
	if err != nil {
		return CallProc{}, err
	}
	out.TruncLimit = int(trunc)
	if out.MinValue, err = f64ArgOr(proc, args, 8, "minValue", 0); err != nil {
		return CallProc{}, err
	}
	if len(args) > 9 {
		if len(args) != 12 {
			return CallProc{}, planErrf("%s filter takes filterProp, filterMin, filterMax together", proc)
		}
		if out.FilterProp, err = strArg(proc, args, 9, "filterProp"); err != nil {
			return CallProc{}, err
		}
		if out.FilterMin, err = i64Arg(proc, args, 10, "filterMin"); err != nil {
			return CallProc{}, err
		}
		if out.FilterMax, err = i64Arg(proc, args, 11, "filterMax"); err != nil {
			return CallProc{}, err
		}
	}
	return out, nil
}

func expectArgCount(proc string, args []value.Value, n int, sig string) error {
	if len(args) != n {
		return planErrf("%s(%s) expects %d arguments, got %d", proc, sig, n, len(args))
	}
	return nil
}

func maxArgs(proc string, args []value.Value, maxN int, sig string) error {
	if len(args) > maxN {
		return planErrf("%s(%s) takes at most %d arguments, got %d", proc, sig, maxN, len(args))
	}
	return nil
}

func strArg(proc string, args []value.Value, idx int, name string) (string, error) {
	if idx < len(args) {
		if s, ok := args[idx].AsStr(); ok {
			return s, nil
		}
	}
	return "", planErrf("%s argument `%s` (position %d) must be a string", proc, name, idx)
}

func f64Arg(proc string, args []value.Value, idx int, name string) (float64, error) {
	if idx < len(args) {
		if f, ok := numArg(args[idx]); ok {
			return f, nil
		}
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a number", proc, name, idx)
}

// numArg widens an int or float value to float64.
func numArg(v value.Value) (float64, bool) {
	if f, ok := v.AsFloat(); ok {
		return f, true
	}
	if i, ok := v.AsInt(); ok {
		return float64(i), true
	}
	return 0, false
}

func f64ArgOr(proc string, args []value.Value, idx int, name string, def float64) (float64, error) {
	if idx >= len(args) || args[idx].IsNull() {
		return def, nil
	}
	return f64Arg(proc, args, idx, name)
}

func boolArgOr(proc string, args []value.Value, idx int, name string, def bool) (bool, error) {
	if idx >= len(args) || args[idx].IsNull() {
		return def, nil
	}
	if b, ok := args[idx].AsBool(); ok {
		return b, nil
	}
	return false, planErrf("%s argument `%s` (position %d) must be a boolean", proc, name, idx)
}

func u32ArgOr(proc string, args []value.Value, idx int, name string, def uint32) (uint32, error) {
	if idx >= len(args) || args[idx].IsNull() {
		return def, nil
	}
	if i, ok := args[idx].AsInt(); ok && i >= 0 {
		return uint32(i), nil
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a non-negative integer", proc, name, idx)
}

func i64Arg(proc string, args []value.Value, idx int, name string) (int64, error) {
	if idx < len(args) {
		if i, ok := args[idx].AsInt(); ok {
			return i, nil
		}
	}
	return 0, planErrf("%s argument `%s` (position %d) must be an integer", proc, name, idx)
}

// nodeArg accepts a bound node or a non-negative integer node id.
func nodeArg(proc string, args []value.Value, idx int, name string) (graph.NodeID, error) {
	if idx < len(args) {
		if n, ok := args[idx].AsNode(); ok {
			return n, nil
		}
		if i, ok := args[idx].AsInt(); ok && i >= 0 {
			return graph.NodeID(i), nil
		}
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a node or a non-negative integer node id", proc, name, idx)
}

// nodesArg accepts a list of nodes/ids or a single node/id.
func nodesArg(proc string, args []value.Value, idx int, name string) ([]graph.NodeID, error) {
	fail := func() ([]graph.NodeID, error) {
		return nil, planErrf("%s argument `%s` (position %d) must be a node/id or a list of them", proc, name, idx)
	}
	if idx >= len(args) {
		return fail()
	}
	if vs, ok := args[idx].AsList(); ok {
		out := make([]graph.NodeID, len(vs))
		for i, v := range vs {
			if n, isNode := v.AsNode(); isNode {
				out[i] = n
			} else if iv, isInt := v.AsInt(); isInt && iv >= 0 {
				out[i] = graph.NodeID(iv)
			} else {
				return fail()
			}
		}
		return out, nil
	}
	n, err := nodeArg(proc, args, idx, name)
	if err != nil {
		return fail()
	}
	return []graph.NodeID{n}, nil
}

// f64sArg accepts a list of numbers or a single number.
func f64sArg(proc string, args []value.Value, idx int, name string) ([]float64, error) {
	fail := func() ([]float64, error) {
		return nil, planErrf("%s argument `%s` (position %d) must be a number or a list of numbers", proc, name, idx)
	}
	if idx >= len(args) {
		return fail()
	}
	if vs, ok := args[idx].AsList(); ok {
		out := make([]float64, len(vs))
		for i, v := range vs {
			f, isNum := numArg(v)
			if !isNum {
				return fail()
			}
			out[i] = f
		}
		return out, nil
	}
	if f, ok := numArg(args[idx]); ok {
		return []float64{f}, nil
	}
	return fail()
}

// strsArg accepts a list of strings or a single string.
func strsArg(proc string, args []value.Value, idx int, name string) ([]string, error) {
	fail := func() ([]string, error) {
		return nil, planErrf("%s argument `%s` (position %d) must be a string or a list of strings", proc, name, idx)
	}
	if idx >= len(args) {
		return fail()
	}
	if vs, ok := args[idx].AsList(); ok {
		out := make([]string, len(vs))
		for i, v := range vs {
			s, isStr := v.AsStr()
			if !isStr {
				return fail()
			}
			out[i] = s
		}
		return out, nil
	}
	if s, ok := args[idx].AsStr(); ok {
		return []string{s}, nil
	}
	return fail()
}

// dirArg parses a direction argument ('both'/'outgoing'/'out'/
// 'incoming'/'in').
func dirArg(proc string, args []value.Value, idx int) (graph.Direction, error) {
	s, err := strArg(proc, args, idx, "direction")
	if err != nil {
		return graph.Both, planErrf("%s direction argument must be a string", proc)
	}
	switch lower(s) {
	case "both":
		return graph.Both, nil
	case "outgoing", "out":
		return graph.Outgoing, nil
	case "incoming", "in":
		return graph.Incoming, nil
	}
	return graph.Both, planErrf("%s direction must be 'both'/'outgoing'/'incoming', got `%s`", proc, s)
}
