// CALL-procedure planning tables: every procedure's argument parsing,
// defaults, and error paths.
package plan

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

func lit(v any) ast.Literal {
	switch x := v.(type) {
	case int:
		return ast.IntLit(int64(x))
	case float64:
		return ast.FloatLit(x)
	case string:
		return ast.StrLit(x)
	case bool:
		return ast.BoolLit(x)
	}
	panic("bad lit")
}

func TestBuildCallProcTable(t *testing.T) {
	cases := []struct {
		proc string
		args []ast.Literal
		want func(p CallProc) bool
	}{
		{"wcc", []ast.Literal{lit("KNOWS")}, func(p CallProc) bool {
			return p.Kind == ProcWcc && p.RelType == "KNOWS" && p.Direction == graph.Both
		}},
		{"wcc", []ast.Literal{lit("KNOWS"), lit("outgoing")}, func(p CallProc) bool {
			return p.Direction == graph.Outgoing
		}},
		{"wcc", []ast.Literal{lit("KNOWS"), lit("in")}, func(p CallProc) bool {
			return p.Direction == graph.Incoming
		}},
		{"fts.search", []ast.Literal{lit("Person"), lit("bio"), lit("hello world")}, func(p CallProc) bool {
			return p.Kind == ProcFtsSearch && p.Label == "Person" && p.Field == "bio" && p.Query == "hello world"
		}},
		{"geo.withinRadius", []ast.Literal{lit("Place"), lit("lat"), lit("lon"), lit(48.8), lit(2.3), lit(10)}, func(p CallProc) bool {
			return p.Kind == ProcGeoWithinRadius && p.Km == 10 && p.Lat == 48.8
		}},
		{"geo.withinBBox", []ast.Literal{lit("Place"), lit("lat"), lit("lon"), lit(1), lit(2), lit(3), lit(4)}, func(p CallProc) bool {
			return p.Kind == ProcGeoWithinBBox && p.MinLat == 1 && p.MaxLon == 4
		}},
		{"algo.bfs", []ast.Literal{lit(7)}, func(p CallProc) bool {
			return p.Kind == ProcBfs && p.Source == 7 && !p.Directed
		}},
		{"algo.bfs", []ast.Literal{lit(7), lit(true)}, func(p CallProc) bool { return p.Directed }},
		{"algo.pagerank", nil, func(p CallProc) bool {
			return p.Kind == ProcPageRank && p.Damping == 0.85 && p.Iters == 20
		}},
		{"algo.pagerank", []ast.Literal{lit(true), lit(0.9), lit(5)}, func(p CallProc) bool {
			return p.Directed && p.Damping == 0.9 && p.Iters == 5
		}},
		{"algo.wcc", nil, func(p CallProc) bool { return p.Kind == ProcWccAll }},
		{"algo.cdlp", []ast.Literal{lit(true), lit(7), lit("vid")}, func(p CallProc) bool {
			return p.Kind == ProcCdlp && p.Directed && p.Iters == 7 && p.SeedProp == "vid"
		}},
		{"algo.cdlp", nil, func(p CallProc) bool { return p.Iters == 10 && p.SeedProp == "" }},
		{"algo.lcc", []ast.Literal{lit(true)}, func(p CallProc) bool {
			return p.Kind == ProcLcc && p.Directed
		}},
		{"algo.sssp", []ast.Literal{lit(3), lit(true), lit(true)}, func(p CallProc) bool {
			return p.Kind == ProcSssp && p.Source == 3 && p.Directed && p.Weighted
		}},
	}
	for _, c := range cases {
		got, err := buildCallProc(c.proc, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.proc, err)
		}
		if !c.want(got) {
			t.Fatalf("%s(%v) = %+v", c.proc, c.args, got)
		}
	}
}

func TestBuildCallProcErrors(t *testing.T) {
	cases := []struct {
		proc string
		args []ast.Literal
		want string
	}{
		{"wcc", nil, "must be a string"},
		{"wcc", []ast.Literal{lit("K"), lit("sideways")}, "'both'/'outgoing'/'incoming'"},
		{"wcc", []ast.Literal{lit("K"), lit(2)}, "must be a string"},
		{"fts.search", []ast.Literal{lit("A")}, "expects 3 arguments"},
		{"geo.withinRadius", []ast.Literal{lit("A")}, "expects 6 arguments"},
		{"geo.withinRadius", []ast.Literal{lit("A"), lit("a"), lit("b"), lit("x"), lit(1), lit(2)}, "must be a number"},
		{"algo.bfs", nil, "node id"},
		{"algo.bfs", []ast.Literal{lit(1), lit(2), lit(3)}, "at most 2"},
		{"algo.bfs", []ast.Literal{lit(1), lit("y")}, "must be a boolean"},
		{"algo.pagerank", []ast.Literal{lit(true), lit(0.9), lit(-1)}, "non-negative integer"},
		{"algo.wcc", []ast.Literal{lit(1)}, "at most 0"},
		{"algo.cdlp", []ast.Literal{lit(true), lit(3), lit(9)}, "must be a string"},
		{"algo.sssp", []ast.Literal{lit(-3)}, "node id"},
		{"mystery.proc", nil, "unknown procedure"},
	}
	for _, c := range cases {
		_, err := buildCallProc(c.proc, c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s(%v): err = %v, want contains %q", c.proc, c.args, err, c.want)
		}
	}
}
