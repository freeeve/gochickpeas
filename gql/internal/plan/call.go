// CALL-procedure planning (port of the Rust plan/call.rs): resolve the
// procedure kind, validate/parse its literal arguments, and map the YIELD
// fields to output slots.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

func buildCallStage(proc string, args []ast.Literal, yields []ast.YieldItem, slots map[string]int, bound map[int]bool, nextSlot *int) (*CallStage, error) {
	kind, err := buildCallProc(proc, args)
	if err != nil {
		return nil, err
	}
	valueField := valueFieldName(&kind)

	nodeSlot, valueSlot := NoSlot, NoSlot
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
		default:
			return nil, planErrf("procedure `%s` does not yield `%s`", proc, y.Field)
		}
	}
	return &CallStage{Proc: kind, NodeSlot: nodeSlot, ValueSlot: valueSlot}, nil
}

// valueFieldName is the per-node scalar column a procedure yields
// alongside node ("" for the search procedures, which yield only node).
func valueFieldName(p *CallProc) string {
	switch p.Kind {
	case ProcWcc:
		return "component"
	case ProcBfs, ProcPageRank, ProcWccAll, ProcCdlp, ProcLcc, ProcSssp:
		return "value"
	}
	return ""
}

// buildCallProc resolves CALL proc(args...) into a validated CallProc.
// Procedure names are case-insensitive; CALL args are never
// auto-parameterized, so they are concrete literals here.
func buildCallProc(proc string, args []ast.Literal) (CallProc, error) {
	switch lower(proc) {
	case "wcc":
		relType, err := strArg(proc, args, 0, "relType")
		if err != nil {
			return CallProc{}, err
		}
		dir := graph.Both
		if len(args) > 1 {
			s, err := strArg(proc, args, 1, "direction")
			if err != nil {
				return CallProc{}, planErrf("wcc direction argument must be a string")
			}
			switch lower(s) {
			case "both":
				dir = graph.Both
			case "outgoing", "out":
				dir = graph.Outgoing
			case "incoming", "in":
				dir = graph.Incoming
			default:
				return CallProc{}, planErrf("wcc direction must be 'both'/'outgoing'/'incoming', got `%s`", s)
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
	}
	return CallProc{}, planErrf("unknown procedure `%s` (supported: wcc, algo.bfs, algo.pagerank, algo.wcc, algo.cdlp, algo.lcc, algo.sssp, fts.search, geo.withinRadius, geo.withinBBox)", proc)
}

func expectArgCount(proc string, args []ast.Literal, n int, sig string) error {
	if len(args) != n {
		return planErrf("%s(%s) expects %d arguments, got %d", proc, sig, n, len(args))
	}
	return nil
}

func maxArgs(proc string, args []ast.Literal, maxN int, sig string) error {
	if len(args) > maxN {
		return planErrf("%s(%s) takes at most %d arguments, got %d", proc, sig, maxN, len(args))
	}
	return nil
}

func strArg(proc string, args []ast.Literal, idx int, name string) (string, error) {
	if idx < len(args) && args[idx].Kind == ast.LitStr {
		return args[idx].S, nil
	}
	return "", planErrf("%s argument `%s` (position %d) must be a string", proc, name, idx)
}

func f64Arg(proc string, args []ast.Literal, idx int, name string) (float64, error) {
	if idx < len(args) {
		switch args[idx].Kind {
		case ast.LitFloat:
			return args[idx].F, nil
		case ast.LitInt:
			return float64(args[idx].I), nil
		}
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a number", proc, name, idx)
}

func f64ArgOr(proc string, args []ast.Literal, idx int, name string, def float64) (float64, error) {
	if idx >= len(args) {
		return def, nil
	}
	return f64Arg(proc, args, idx, name)
}

func boolArgOr(proc string, args []ast.Literal, idx int, name string, def bool) (bool, error) {
	if idx >= len(args) {
		return def, nil
	}
	if args[idx].Kind == ast.LitBool {
		return args[idx].B, nil
	}
	return false, planErrf("%s argument `%s` (position %d) must be a boolean", proc, name, idx)
}

func u32ArgOr(proc string, args []ast.Literal, idx int, name string, def uint32) (uint32, error) {
	if idx >= len(args) {
		return def, nil
	}
	if args[idx].Kind == ast.LitInt && args[idx].I >= 0 {
		return uint32(args[idx].I), nil
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a non-negative integer", proc, name, idx)
}

func nodeArg(proc string, args []ast.Literal, idx int, name string) (graph.NodeID, error) {
	if idx < len(args) && args[idx].Kind == ast.LitInt && args[idx].I >= 0 {
		return graph.NodeID(args[idx].I), nil
	}
	return 0, planErrf("%s argument `%s` (position %d) must be a non-negative integer node id", proc, name, idx)
}
