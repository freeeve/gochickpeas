// Temporal/duration overflow yields Null, never a silently wrapped instant
// (rcp twin 457c858). Go's integer overflow wraps in every build, so these
// assert the value, and the compiled fast path stays identical to the tree.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// scalarVal runs a one-row query and returns the single column's value.
func scalarVal(t *testing.T, g *chickpeas.Snapshot, q, col string) (v value.Value, rows int) {
	t.Helper()
	res, err := Run(g, q)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", q, err)
	}
	for r := range res.All() {
		rows++
		v, _ = r.Get(col)
	}
	return v, rows
}

func TestDurationOverflowYieldsNull(t *testing.T) {
	g := socialGraph(t)
	anchor := "MATCH (p:Person {name: 'Alice'}) RETURN "
	// The rcp ask's example query: a days count that overflows the tick
	// conversion returns Null, not a confidently wrong date.
	if v, n := scalarVal(t, g, anchor+"date('2020-01-01') + duration({days: 9223372036854775807}) AS d", "d"); n != 1 || !v.IsNull() {
		t.Fatalf("overflowing add: rows=%d null=%v (want 1 row, Null)", n, v.IsNull())
	}
	// ISO-string and calendar-month overflows decline the same way.
	if v, _ := scalarVal(t, g, anchor+"date('2020-01-01') + duration('P9223372036854775807Y') AS d", "d"); !v.IsNull() {
		t.Fatalf("overflowing ISO years: got %v, want Null", v)
	}
	// A representable shift stays a value.
	if v, _ := scalarVal(t, g, anchor+"date('2020-01-01') - duration({days: 100}) AS d", "d"); v.IsNull() {
		t.Fatal("representable subtraction became Null")
	}
	// A datetime built from an absurd year is Null, not a wrapped instant.
	if v, _ := scalarVal(t, g, anchor+"datetime({year: 300000001, month: 1, day: 1}) AS d", "d"); !v.IsNull() {
		t.Fatalf("absurd-year datetime: got %v, want Null", v)
	}
}

// toString on a KindTemporal (datetime / localdatetime) renders ISO-8601
// (rcp twin a8a13e9): previously it fell through applyToString's catch-all
// and returned a silent Null in the projection. A calendar shift keeps the
// kind, so the shifted instant stringifies too. Duration stays Null.
func TestToStringTemporal(t *testing.T) {
	g := socialGraph(t)
	anchor := "MATCH (p:Person {name: 'Alice'}) RETURN "
	cases := []struct{ q, want string }{
		{anchor + "toString(datetime('2020-01-01T13:45:30')) AS s", "2020-01-01T13:45:30"},
		{anchor + "toString(datetime('2020-01-01T00:00:00') + duration({days: 31})) AS s", "2020-02-01T00:00:00"},
		{anchor + "toString(localdatetime('2020-06-15T08:30:00.250')) AS s", "2020-06-15T08:30:00.250"},
		{anchor + "toString(datetime('2020-01-01')) AS s", "2020-01-01T00:00:00"},
	}
	for _, c := range cases {
		v, n := scalarVal(t, g, c.q, "s")
		if n != 1 {
			t.Fatalf("%s: %d rows", c.q, n)
		}
		if s, _ := v.AsStr(); s != c.want {
			t.Fatalf("%s = %q, want %q", c.q, s, c.want)
		}
	}
	// A duration has no single string form here, so it stays Null (a
	// deliberate divergence point from the sibling engine, noted in 080).
	if v, _ := scalarVal(t, g, anchor+"toString(duration({days: 5})) AS s", "s"); !v.IsNull() {
		t.Fatalf("toString(duration) = %v, want Null", v)
	}
}

// eventGraph carries two epoch-millis columns so a comparison shifted by a
// duration takes the whole-row i64 fast path.
func eventGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 1)
	for _, ts := range []int64{1_577_836_800_000, 1_580_515_200_000} { // 2020-01-01, 2020-02-01
		id, _ := b.AddNode("Event")
		if err := b.SetProp(id, "t", ts); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("t")
}

// The compiled whole-row fast path folds a months-free duration to a tick
// offset; an overflowing constant must decline the specialization so the
// tree's checked ApplyDuration produces the same Null (runBoth asserts the
// compiled and interpreted paths agree row-for-row).
func TestDurationFastPathOverflowMatchesTree(t *testing.T) {
	g := eventGraph(t)
	// b.t + duration({days: MaxInt64}) overflows the fold: the comparison is
	// Null for every pair, so no rows survive on either path.
	rows := runBoth(t, g,
		"MATCH (a:Event), (b:Event) WHERE a.t < b.t + duration({days: 9223372036854775807}) RETURN a.t AS at")
	n := 0
	for range rows.All() {
		n++
	}
	if n != 0 {
		t.Fatalf("overflowing fast-path shift kept %d rows, want 0", n)
	}
	// A representable shift still filters normally: a.t < b.t + 10 days keeps
	// the (earlier, later) ordering pairs.
	rows2 := runBoth(t, g,
		"MATCH (a:Event), (b:Event) WHERE a.t < b.t + duration({days: 10}) RETURN a.t AS at")
	n = 0
	for range rows2.All() {
		n++
	}
	if n == 0 {
		t.Fatal("representable fast-path shift filtered everything")
	}
}
