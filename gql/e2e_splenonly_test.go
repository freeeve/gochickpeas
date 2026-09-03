// Shortest-path materialization elision end-to-end: the length-only
// execution (integer distance, no path assembly) against the
// materialized-path pipeline, row-for-row in engine order. The fixture
// exercises both stage branches -- a source shared by many rows (one
// frontier walk answers all targets) and single-row sources (early-exit
// bidirectional search) -- plus unreachable pairs and the OPTIONAL form.
package gql_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// spLenGraph: two KNOWS components -- a 5-person chain 0-1-2-3-4 with a
// shortcut 0-2, and an isolated pair 5-6 -- so distances vary (dist(0,3)
// = 2 via the shortcut) and cross-component pairs have no path.
func spLenGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 16)
	for i := 0; i < 7; i++ {
		n, err := b.AddNode("P")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "pid", int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {0, 2}, {5, 6}} {
		if _, err := b.AddRel(e[0], e[1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("splen-e2e")
}

// spLenBoth runs q with and without the elision and asserts identical
// ordered rows (canonical group-key encoding), returning them.
func spLenBoth(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	elided := hjRowKeysOrdered(t, g, q)
	plan.DisableSpLenOnly = true
	defer func() { plan.DisableSpLenOnly = false }()
	materialized := hjRowKeysOrdered(t, g, q+" ")
	if !slices.Equal(elided, materialized) {
		t.Fatalf("ordered-row divergence on %s:\nlength-only  (%d): %v\nmaterialized (%d): %v",
			q, len(elided), elided, len(materialized), materialized)
	}
	return elided
}

// intOrNullCol reads an integer column in result order; nulls become -1.
func intOrNullCol(t *testing.T, g *chickpeas.Snapshot, q, col string) []int64 {
	t.Helper()
	rows, err := gql.Run(g, q)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", q, err)
	}
	var out []int64
	for r := range rows.All() {
		v, _ := r.Get(col)
		if v.IsNull() {
			out = append(out, -1)
			continue
		}
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("column %q not int/null in %s: %v", col, q, v)
		}
		out = append(out, i)
	}
	return out
}

func TestSpLenOnlyMultiTargetSource(t *testing.T) {
	g := spLenGraph(t)
	// One source (pid 0) against every other person: the shared-source
	// branch, with unreachable targets (5, 6) dropped.
	q := `MATCH (s:P {pid: 0}) MATCH (e:P) FILTER e.pid <> 0 RETURN s, e
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 LET dist = length(p) FILTER dist >= 1
		 RETURN e.pid AS pid, dist AS dist ORDER BY pid`
	spLenBoth(t, g, q)
	if got, want := intCol(t, g, q, "pid"), []int64{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("pids wrong: got %v, want %v", got, want)
	}
	// The 0-2 shortcut makes dist(0,3) 2, not 3.
	if got, want := intCol(t, g, q, "dist"), []int64{1, 1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("distances wrong: got %v, want %v", got, want)
	}
}

func TestSpLenOnlySingleRowSources(t *testing.T) {
	g := spLenGraph(t)
	// Distinct source per row: the early-exit bidirectional branch.
	rows := spLenBoth(t, g,
		`MATCH (a:P)-[:KNOWS]-(b:P) FILTER a.pid < b.pid RETURN a, b
		 NEXT MATCH p = ANY SHORTEST (a)-[:KNOWS]-{1,4}(b)
		 RETURN a.pid AS apid, b.pid AS bpid, length(p) AS dist ORDER BY apid, bpid`)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
}

func TestSpLenOnlyOptionalUnreachable(t *testing.T) {
	g := spLenGraph(t)
	// OPTIONAL keeps unreachable pairs with a null distance.
	q := `MATCH (s:P {pid: 0}) MATCH (e:P {pid: 5}) RETURN s, e
		 NEXT OPTIONAL MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 RETURN e.pid AS pid, length(p) AS dist`
	spLenBoth(t, g, q)
	if got, want := intOrNullCol(t, g, q, "dist"), []int64{-1}; !slices.Equal(got, want) {
		t.Fatalf("optional-null distance wrong: got %v, want null", got)
	}
}

func TestSpLenOnlyFilterDropsShort(t *testing.T) {
	g := spLenGraph(t)
	// The Q10 idiom: aggregate over targets at distance >= 2 only.
	q := `MATCH (s:P {pid: 0}) MATCH (e:P) FILTER e.pid <> 0 RETURN s, e
		 NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		 LET dist = length(p) FILTER dist >= 2
		 RETURN count(*) AS far`
	spLenBoth(t, g, q)
	if got, want := intCol(t, g, q, "far"), []int64{2}; !slices.Equal(got, want) {
		t.Fatalf("aggregate wrong: got %v, want %v", got, want)
	}
}
