// FOR-over-collected-nodes regression (task 204, a ragedb cross-engine
// find): `collect a node list NEXT FOR x IN list MATCH (x)-...` doubled
// rows per shard on their engine and dropped list-held nodes' properties
// (x.key read null). Our engine has neither sharding nor lazy list-node
// materialization, but the shape is pinned exactly: aggregate counts
// must be per-edge exact and a FOR-bound node's properties must read
// through both the literal and cached paths.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func forCollectedFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	f1, err := b.AddNode("Forum")
	must(err)
	must(b.SetProp(f1, "title", "F1"))
	f2, err := b.AddNode("Forum")
	must(err)
	must(b.SetProp(f2, "title", "F2"))
	p1, err := b.AddNode("Person")
	must(err)
	must(b.SetProp(p1, "name", "P1"))
	p2, err := b.AddNode("Person")
	must(err)
	must(b.SetProp(p2, "name", "P2"))
	_, err = b.AddRel(f1, p1, "HAS_MEMBER")
	must(err)
	_, err = b.AddRel(f1, p2, "HAS_MEMBER")
	must(err)
	_, err = b.AddRel(f2, p1, "HAS_MEMBER")
	must(err)
	return b.Finalize("for-collected")
}

func TestForOverCollectedNodes(t *testing.T) {
	g := forCollectedFixture(t)
	for name, run := range map[string]func(*chickpeas.Snapshot, string) (*Rows, error){
		"uncached": RunUncached, "cached": Run,
	} {
		t.Run(name, func(t *testing.T) {
			// Membership counts: P1 in both forums (2), P2 in one (1) --
			// the shape ragedb saw doubled per shard.
			rows, err := run(g, "MATCH (f:Forum) RETURN collect_list(f) AS forums NEXT FOR ff IN forums MATCH (ff)-[:HAS_MEMBER]->(p:Person) RETURN p.name AS name, sum(1) AS total ORDER BY name")
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int64{"P1": 2, "P2": 1}
			n := 0
			for r := range rows.All() {
				nm, _ := r.Values()[0].AsStr()
				tot, _ := r.Values()[1].AsInt()
				if want[nm] != tot {
					t.Fatalf("%s total = %d, want %d", nm, tot, want[nm])
				}
				n++
			}
			if n != 2 {
				t.Fatalf("rows = %d, want 2", n)
			}
			// Property reads on the FOR-bound list node -- the half ragedb
			// saw as null.
			rows, err = run(g, "MATCH (f:Forum) RETURN collect_list(f) AS forums NEXT FOR ff IN forums MATCH (ff)-[:HAS_MEMBER]->(p:Person) RETURN ff.title AS ft, p.name AS name ORDER BY ft, name")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for r := range rows.All() {
				ft, ok := r.Values()[0].AsStr()
				if !ok || ft == "" {
					t.Fatal("ff.title read empty/null on a list-held node")
				}
				nm, _ := r.Values()[1].AsStr()
				got = append(got, ft+":"+nm)
			}
			wantRows := []string{"F1:P1", "F1:P2", "F2:P1"}
			if len(got) != len(wantRows) {
				t.Fatalf("rows = %v, want %v", got, wantRows)
			}
			for i := range wantRows {
				if got[i] != wantRows[i] {
					t.Fatalf("row[%d] = %s, want %s", i, got[i], wantRows[i])
				}
			}
		})
	}
}
