// Group-join decorrelation fixtures: the rewrite (an OPTIONAL MATCH
// consumed only through qualifying aggregates becomes a standalone inner
// plus a keyed left join) compared against the nested execution
// row-for-row -- zero-fill groups, duplicate outer rows, every supported
// aggregate's fill, and the decline set (count(*), outer-only aggregates,
// inner variables escaping, WHERE reading beyond the pattern). The
// breadth floor is lowered per forced run so fixture-scale outers
// qualify; the default-floor case pins the gate itself.
package gql

import (
	"slices"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// gjRowKey encodes expected values exactly like hjRowKeys encodes a
// result row, so content assertions compare whole rows.
func gjRowKey(vs ...value.Value) string {
	var sb strings.Builder
	for _, v := range vs {
		sb.Write(value.AppendKey(nil, v))
		sb.WriteByte('|')
	}
	return sb.String()
}

// gjCompare runs q nested (default breadth floor), re-plans a textual
// variant with the floor lowered to zero, requires the rewrite to fire
// (or not, per wantJoin), and requires identical row multisets. Returns
// the multiset for content assertions.
func gjCompare(t *testing.T, g *chickpeas.Snapshot, q string, wantJoin bool) []string {
	t.Helper()
	nested := hjRowKeys(t, g, q)
	floor := plan.GroupJoinMinOuterRows
	plan.GroupJoinMinOuterRows = 0
	defer func() { plan.GroupJoinMinOuterRows = floor }()
	qf := q + " " // defeat the plan cache: the lowered floor must re-plan
	pl, err := Explain(g, qf)
	if err != nil {
		t.Fatalf("explain failed: %s\n%v", qf, err)
	}
	if got := strings.Contains(pl, "GroupJoin"); got != wantJoin {
		t.Fatalf("GroupJoin in forced plan = %v, want %v:\n%s", got, wantJoin, pl)
	}
	joined := hjRowKeys(t, g, qf)
	if !slices.Equal(nested, joined) {
		t.Fatalf("row multiset divergence for %s\nnested (%d): %v\njoined (%d): %v\nplan:\n%s",
			q, len(nested), nested, len(joined), joined, pl)
	}
	return nested
}

// groupJoinGraph: six :U outers; u0 has R-children m with v in {1,2,3},
// u1 has one with v=5, u2..u5 have none (zero-fill groups). A :W decoy
// with an R edge pins that the inner's label filter still applies.
func groupJoinGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(32, 64)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var us []chickpeas.NodeID
	for i := range 6 {
		u, err := b.AddNode("U")
		must(err)
		must(b.SetProp(u, "name", "u"+string(rune('0'+i))))
		us = append(us, u)
	}
	addM := func(u chickpeas.NodeID, v int64) {
		m, err := b.AddNode("M")
		must(err)
		must(b.SetProp(m, "v", v))
		_, err = b.AddRel(m, u, "R")
		must(err)
	}
	addM(us[0], 1)
	addM(us[0], 2)
	addM(us[0], 3)
	addM(us[1], 5)
	w, err := b.AddNode("W")
	must(err)
	must(b.SetProp(w, "v", int64(99)))
	_, err = b.AddRel(w, us[2], "R")
	must(err)
	return b.Finalize("groupjoin-fixture")
}

func TestGroupJoinAggregates(t *testing.T) {
	g := groupJoinGraph(t)
	rows := gjCompare(t, g,
		"MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) WHERE m.v < 10 RETURN u.name AS un, count(m) AS c, sum(m.v) AS s, min(m.v) AS mn, max(m.v) AS mx ORDER BY un",
		true)
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 (one per outer)", len(rows))
	}
	// u0: three matches; u1: one; u2 (the :W decoy's target) through u5:
	// zero matches -- count and sum 0 (sum's empty-group identity is 0,
	// Cypher/GQL semantics), min/max null.
	if want := gjRowKey(value.Str("u0"), value.Int(3), value.Int(6), value.Int(1), value.Int(3)); rows[0] != want {
		t.Fatalf("u0 row = %q, want count 3, sum 6, min 1, max 3", rows[0])
	}
	if want := gjRowKey(value.Str("u1"), value.Int(1), value.Int(5), value.Int(5), value.Int(5)); rows[1] != want {
		t.Fatalf("u1 row = %q, want count 1, sum/min/max 5", rows[1])
	}
	for i, r := range rows[2:] {
		un := "u" + string(rune('2'+i))
		if want := gjRowKey(value.Str(un), value.Int(0), value.Int(0), value.Null(), value.Null()); r != want {
			t.Fatalf("zero-fill row %s = %q, want count 0, sum 0, min/max null", un, r)
		}
	}
}

// TestGroupJoinDuplicateOuters pins duplicate-decomposability: two
// duplicate outer rows per group (a FOR cross join) pool their joined
// rows in the nested execution, and the re-aggregation of the synthetic
// columns reproduces exactly that.
func TestGroupJoinDuplicateOuters(t *testing.T) {
	g := groupJoinGraph(t)
	rows := gjCompare(t, g,
		"FOR d IN [1, 2] MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(m) AS c, sum(m.v) AS s ORDER BY un",
		true)
	// Grouped by u.name only: each group holds both duplicate outer rows,
	// so u0 counts 6 (3 matches x 2 dups) and sums 12.
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 groups", len(rows))
	}
	if want := gjRowKey(value.Str("u0"), value.Int(6), value.Int(12)); rows[0] != want {
		t.Fatalf("u0 row = %q, want count 6, sum 12 over duplicate outers", rows[0])
	}
}

// TestGroupJoinDeclines pins the decline set: each shape must plan
// WITHOUT the rewrite even when the breadth floor is zero, and stay
// nested-identical (trivially, since both paths are the nested one).
func TestGroupJoinDeclines(t *testing.T) {
	g := groupJoinGraph(t)
	for _, tc := range []struct {
		name, q string
	}{
		{"count-star", "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(*) AS c ORDER BY un"},
		{"outer-only-agg", "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(u) AS c ORDER BY un"},
		{"inner-var-escapes", "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN m.v AS mv, count(m) AS c ORDER BY mv"},
		{"agg-in-arithmetic", "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(m) + 1 AS c ORDER BY un"},
		{"distinct-agg", "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(DISTINCT m) AS c ORDER BY un"},
		{"where-reads-outside", "FOR lim IN [10] MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) WHERE m.v < lim RETURN u.name AS un, count(m) AS c ORDER BY un"},
		{"uncorrelated", "MATCH (u:U) OPTIONAL MATCH (m:M)-[:R]->(x:U) RETURN u.name AS un, count(m) AS c ORDER BY un"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gjCompare(t, g, tc.q, false)
		})
	}
}

// TestGroupJoinBreadthGate pins the dual: a fixture-scale outer (six
// rows) stays NESTED under the default floor -- the rewrite only pays
// when the outer is broad enough that the nested walk multiplies.
func TestGroupJoinBreadthGate(t *testing.T) {
	g := groupJoinGraph(t)
	pl, err := Explain(g, "MATCH (u:U) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(m) AS c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pl, "GroupJoin") {
		t.Fatalf("six outer rows rewrote under the default breadth floor:\n%s", pl)
	}
}

// TestGroupJoinCoverageGate pins the other half of the economics: a
// SELECTIVE outer (one seeked node of six) keeps the nested walk even
// with the breadth floor forced to zero -- the standalone inner would
// enumerate every :U's children while the nested walk expands one.
func TestGroupJoinCoverageGate(t *testing.T) {
	g := groupJoinGraph(t)
	gjCompare(t, g,
		"MATCH (u:U {name: 'u0'}) OPTIONAL MATCH (u)<-[:R]-(m:M) RETURN u.name AS un, count(m) AS c",
		false)
}
