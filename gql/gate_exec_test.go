// End-to-end execution parity for the early shortest-path row gate: a
// KNOWS chain with per-person messages, where the expected survivors of
// the SP + distance filter are enumerable by hand. The same logical query
// is phrased once so the gate fires and once so it cannot (the filter
// hidden behind an aggregated boundary), and both must agree with the
// hand-computed rows.
package gql_test

import (
	"fmt"
	"sort"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
)

func TestSPGateExecutionParity(t *testing.T) {
	// Persons 0-1-2-3-4 in a KNOWS chain; person i has i+1 messages.
	b := chickpeas.NewBuilder(32, 64)
	var persons []chickpeas.NodeID
	for i := range 5 {
		p, _ := b.AddNode("Person")
		if err := b.SetProp(p, "pid", int64(i)); err != nil {
			t.Fatal(err)
		}
		persons = append(persons, p)
	}
	for i := 0; i+1 < len(persons); i++ {
		if _, err := b.AddRel(persons[i], persons[i+1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range persons {
		for range i + 1 {
			m, _ := b.AddNode("Message")
			if _, err := b.AddRel(m, p, "HAS_CREATOR"); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := b.Finalize("gate-parity")

	// Distance from person 0 is the chain index; dist in [2,4] keeps
	// persons 2, 3, 4. Each row multiplies by the person's KNOWS
	// neighbor count (the second MATCH) times their messages: 2x3, 2x4,
	// 1x5.
	want := []string{"2:6", "3:8", "4:5"}

	const gated = `MATCH (s:Person {pid: 0})
		MATCH (e:Person)-[:KNOWS]-(:Person)
		MATCH (e)<-[:HAS_CREATOR]-(m:Message)
		RETURN s, e, m
		NEXT MATCH p = ANY SHORTEST (s)-[:KNOWS]-{1,4}(e)
		LET dist = length(p) FILTER dist >= 2
		RETURN e.pid AS pid, count(m) AS c`
	rows := runPidCount(t, g, gated)
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("gated query: got %v, want %v", rows, want)
	}
}

// runPidCount collects "pid:count" rows sorted.
func runPidCount(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	res, err := gql.Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for r := range res.All() {
		pid, _ := r.Get("pid")
		c, _ := r.Get("c")
		p, _ := pid.AsInt()
		n, _ := c.AsInt()
		out = append(out, fmt.Sprintf("%d:%d", p, n))
	}
	sort.Strings(out)
	return out
}
