// Marking matrix for the rel-list length elision: a var-expand's bound
// rel list read only as size(x) -- including through the named-path
// elision's synthetic column and LET/FILTER boundary carries -- marks
// RelLenOnly and rewrites the reads; any other read of the list refuses.
// Pinned at the plan so the exec differential cannot pass vacuously.
package plan

import (
	"testing"
)

func relLenDelta(t *testing.T, src string) int {
	t.Helper()
	g := elideFixture(t)
	before := relLenElides
	mustPlan(t, graphNew(g), src)
	return relLenElides - before
}

func TestRelLenOnlyFires(t *testing.T) {
	for _, src := range []string{
		// The CR1 shape: named path over one quantified hop, timestamp
		// comprehension, monotonic filter, min(size(ts)) aggregate. The
		// path elision and dead-LET reductions leave exactly one
		// size(<rel-list column>) read for this pass.
		"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) LET ts = [r IN rels(p) | r.ct] FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) RETURN o.aid AS oid, min(size(ts)) AS dist ORDER BY oid",
		// Direct named rel list read as size only.
		"MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) RETURN o.aid AS oid, size(e) AS n ORDER BY oid, n",
	} {
		if d := relLenDelta(t, src); d != 1 {
			t.Fatalf("relLenElides moved %d on %q, want 1", d, src)
		}
	}
}

func TestRelLenOnlyDeclines(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"element-read",
			"MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) RETURN o.aid AS oid, size(e) AS n, e[0] AS first ORDER BY oid, n"},
		{"iteration-with-values",
			"MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) LET ts = [r IN e | r.ct] RETURN o.aid AS oid, ts AS ts"},
		{"bare-projection",
			"MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) RETURN e AS e"},
		{"live-path-bind",
			"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) RETURN o.aid AS oid, size(nodes(p)) AS n ORDER BY oid, n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := relLenDelta(t, tc.src); d != 0 {
				t.Fatalf("relLenElides moved %d, want 0", d)
			}
		})
	}
}

func TestRelLenOnlyDisableSwitch(t *testing.T) {
	DisableRelLenOnly = true
	defer func() { DisableRelLenOnly = false }()
	src := "MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) RETURN o.aid AS oid, size(e) AS n ORDER BY oid, n"
	if d := relLenDelta(t, src); d != 0 {
		t.Fatalf("relLenElides moved %d with the pass disabled, want 0", d)
	}
}
