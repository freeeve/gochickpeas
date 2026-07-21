// Argument coercion and validation helpers for procedure resolution: each
// reads one positional CALL argument (or a list form) as a typed value,
// returning a plan error naming the procedure, argument, and position on a
// mismatch. Split from call.go, which holds the CALL stage building and
// procedure resolution.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

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
