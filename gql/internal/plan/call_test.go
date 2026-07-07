// CALL-procedure planning tables: every procedure's argument parsing,
// defaults, and error paths.
package plan

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func lit(v any) value.Value {
	switch x := v.(type) {
	case int:
		return value.Int(int64(x))
	case float64:
		return value.Float(x)
	case string:
		return value.Str(x)
	case bool:
		return value.Bool(x)
	case []any:
		vs := make([]value.Value, len(x))
		for i, e := range x {
			vs[i] = lit(e)
		}
		return value.List(vs)
	}
	panic("bad lit")
}

func TestBuildCallProcTable(t *testing.T) {
	cases := []struct {
		proc string
		args []value.Value
		want func(p CallProc) bool
	}{
		{"wcc", []value.Value{lit("KNOWS")}, func(p CallProc) bool {
			return p.Kind == ProcWcc && p.RelType == "KNOWS" && p.Direction == graph.Both
		}},
		{"wcc", []value.Value{lit("KNOWS"), lit("outgoing")}, func(p CallProc) bool {
			return p.Direction == graph.Outgoing
		}},
		{"wcc", []value.Value{lit("KNOWS"), lit("in")}, func(p CallProc) bool {
			return p.Direction == graph.Incoming
		}},
		{"fts.search", []value.Value{lit("Person"), lit("bio"), lit("hello world")}, func(p CallProc) bool {
			return p.Kind == ProcFtsSearch && p.Label == "Person" && p.Field == "bio" && p.Query == "hello world"
		}},
		{"geo.withinRadius", []value.Value{lit("Place"), lit("lat"), lit("lon"), lit(48.8), lit(2.3), lit(10)}, func(p CallProc) bool {
			return p.Kind == ProcGeoWithinRadius && p.Km == 10 && p.Lat == 48.8
		}},
		{"geo.withinBBox", []value.Value{lit("Place"), lit("lat"), lit("lon"), lit(1), lit(2), lit(3), lit(4)}, func(p CallProc) bool {
			return p.Kind == ProcGeoWithinBBox && p.MinLat == 1 && p.MaxLon == 4
		}},
		{"algo.bfs", []value.Value{lit(7)}, func(p CallProc) bool {
			return p.Kind == ProcBfs && p.Source == 7 && !p.Directed
		}},
		{"algo.bfs", []value.Value{lit(7), lit(true)}, func(p CallProc) bool { return p.Directed }},
		{"algo.pagerank", nil, func(p CallProc) bool {
			return p.Kind == ProcPageRank && p.Damping == 0.85 && p.Iters == 20
		}},
		{"algo.pagerank", []value.Value{lit(true), lit(0.9), lit(5)}, func(p CallProc) bool {
			return p.Directed && p.Damping == 0.9 && p.Iters == 5
		}},
		{"algo.wcc", nil, func(p CallProc) bool { return p.Kind == ProcWccAll }},
		{"algo.cdlp", []value.Value{lit(true), lit(7), lit("vid")}, func(p CallProc) bool {
			return p.Kind == ProcCdlp && p.Directed && p.Iters == 7 && p.SeedProp == "vid"
		}},
		{"algo.cdlp", nil, func(p CallProc) bool { return p.Iters == 10 && p.SeedProp == "" }},
		{"algo.lcc", []value.Value{lit(true)}, func(p CallProc) bool {
			return p.Kind == ProcLcc && p.Directed
		}},
		{"algo.sssp", []value.Value{lit(3), lit(true), lit(true)}, func(p CallProc) bool {
			return p.Kind == ProcSssp && p.Source == 3 && p.Directed && p.Weighted
		}},
		{"algo.propagate", []value.Value{lit([]any{1, 2}), lit([]any{5.0, 3.0}), lit([]any{"transfer", "withdraw"}), lit("out"), lit(3), lit("amount"), lit("asc"), lit(10000)}, func(p CallProc) bool {
			return p.Kind == ProcPropagate && len(p.Seeds) == 2 && p.Seeds[1] == 2 &&
				p.SeedVals[0] == 5.0 && len(p.RelTypes) == 2 && p.Direction == graph.Outgoing &&
				p.MaxDepth == 3 && p.ValueProp == "amount" && !p.Desc && p.TruncLimit == 10000 &&
				p.MinValue == 0 && p.FilterProp == ""
		}},
		{"algo.propagate", []value.Value{lit(1), lit(5.0), lit("flow"), lit("both"), lit(2), lit("w"), lit("desc"), lit(0), lit(-1.5), lit("ts"), lit(10), lit(20)}, func(p CallProc) bool {
			return len(p.Seeds) == 1 && len(p.RelTypes) == 1 && p.Desc && p.TruncLimit == 0 &&
				p.MinValue == -1.5 && p.FilterProp == "ts" && p.FilterMin == 10 && p.FilterMax == 20
		}},
	}
	for _, c := range cases {
		got, err := ResolveCallProc(c.proc, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.proc, err)
		}
		if !c.want(got) {
			t.Fatalf("%s(%v) = %+v", c.proc, c.args, got)
		}
	}
}

func TestBuildCallProcErrors(t *testing.T) {
	okProp := []value.Value{lit([]any{1}), lit([]any{5.0}), lit("flow"), lit("out"), lit(3), lit("amt"), lit("asc"), lit(0)}
	prop := func(idx int, v value.Value) []value.Value {
		out := append([]value.Value{}, okProp...)
		for len(out) <= idx {
			out = append(out, lit(0))
		}
		out[idx] = v
		return out
	}
	cases := []struct {
		proc string
		args []value.Value
		want string
	}{
		{"wcc", nil, "must be a string"},
		{"wcc", []value.Value{lit("K"), lit("sideways")}, "'both'/'outgoing'/'incoming'"},
		{"wcc", []value.Value{lit("K"), lit(2)}, "must be a string"},
		{"fts.search", []value.Value{lit("A")}, "expects 3 arguments"},
		{"geo.withinRadius", []value.Value{lit("A")}, "expects 6 arguments"},
		{"geo.withinRadius", []value.Value{lit("A"), lit("a"), lit("b"), lit("x"), lit(1), lit(2)}, "must be a number"},
		{"algo.bfs", nil, "node id"},
		{"algo.bfs", []value.Value{lit(1), lit(2), lit(3)}, "at most 2"},
		{"algo.bfs", []value.Value{lit(1), lit("y")}, "must be a boolean"},
		{"algo.pagerank", []value.Value{lit(true), lit(0.9), lit(-1)}, "non-negative integer"},
		{"algo.wcc", []value.Value{lit(1)}, "at most 0"},
		{"algo.cdlp", []value.Value{lit(true), lit(3), lit(9)}, "must be a string"},
		{"algo.sssp", []value.Value{lit(-3)}, "node id"},
		{"mystery.proc", nil, "unknown procedure"},
		{"algo.propagate", okProp[:7], "at least 8 arguments"},
		{"algo.propagate", prop(0, lit("x")), "list of them"},
		{"algo.propagate", []value.Value{lit([]any{1, 2}), lit([]any{5.0}), lit("flow"), lit("out"), lit(3), lit("amt"), lit("asc"), lit(0)}, "same length"},
		{"algo.propagate", prop(1, lit("x")), "list of numbers"},
		{"algo.propagate", prop(2, lit(3)), "list of strings"},
		{"algo.propagate", prop(3, lit("sideways")), "'both'/'outgoing'/'incoming'"},
		{"algo.propagate", prop(4, lit(-2)), "non-negative integer"},
		{"algo.propagate", prop(6, lit("sideways")), "'asc' or 'desc'"},
		{"algo.propagate", append(append([]value.Value{}, okProp...), lit(0.0), lit("ts")), "together"},
	}
	for _, c := range cases {
		_, err := ResolveCallProc(c.proc, c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s(%v): err = %v, want contains %q", c.proc, c.args, err, c.want)
		}
	}
}
