// Marking matrix for named-path elision: the qualifying shape (one
// quantified hop, every use rels(p)/length(p), including uses reached
// through LET/FILTER boundary carries) rewrites and drops the assembly;
// every other use of the path refuses -- pinned at the plan so the exec
// differential cannot pass vacuously.
package plan

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func elideFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	var ids []chickpeas.NodeID
	for i := range 5 {
		n, _ := b.AddNode("A")
		_ = b.SetProp(n, "aid", int64(i))
		ids = append(ids, n)
	}
	for i := 0; i < 4; i++ {
		r, err := b.AddRel(ids[i+1], ids[i], "T")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(r, "ct", int64(100-i)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("elide")
}

func elideDelta(t *testing.T, g *chickpeas.Snapshot, src string) int {
	t.Helper()
	before := pathElides
	mustPlan(t, graphNew(g), src)
	return pathElides - before
}

func TestPathElideFires(t *testing.T) {
	g := elideFixture(t)
	for _, src := range []string{
		// Same-segment uses only.
		"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) WHERE size(rels(p)) > 0 RETURN o.aid AS oid, length(p) AS l ORDER BY oid, l",
		// The CR1 shape: uses reached through LET and FILTER boundary
		// carries, ending in an aggregate over the carried column.
		"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) LET ts = [r IN rels(p) | r.ct] FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) RETURN o.aid AS oid, min(size(ts)) AS dist ORDER BY oid",
	} {
		if d := elideDelta(t, g, src); d != 1 {
			t.Fatalf("pathElides moved %d on %q, want 1", d, src)
		}
	}
}

func TestPathElideDeclines(t *testing.T) {
	g := elideFixture(t)
	for _, tc := range []struct{ name, src string }{
		{"nodes-use",
			"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) RETURN size(nodes(p)) AS n, o.aid AS oid ORDER BY oid"},
		{"bare-projection",
			"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) RETURN p AS p"},
		{"fixed-hop",
			"MATCH p = (s:A {aid: 0})<-[:T]-(o:A) RETURN size(rels(p)) AS n, o.aid AS oid ORDER BY oid"},
		{"carried-to-output",
			"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) LET l = length(p) RETURN p AS p, l AS l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := elideDelta(t, g, tc.src); d != 0 {
				t.Fatalf("pathElides moved %d, want 0", d)
			}
		})
	}
}

func TestPathElideDisableSwitch(t *testing.T) {
	g := elideFixture(t)
	DisablePathElide = true
	defer func() { DisablePathElide = false }()
	if d := elideDelta(t, g,
		"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) RETURN o.aid AS oid, length(p) AS l ORDER BY oid, l"); d != 0 {
		t.Fatalf("elision fired under DisablePathElide (%d)", d)
	}
}
