package gql

import (
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// monoTrailGraph builds three independent decreasing-trail probes off
// distinct anchors, each exercising a different edge of the monotonic
// pushdown's equivalence to the all()/range filter.
func monoTrailGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(9, 6)
	for _, n := range []string{"s", "a", "b", "s2", "x", "y", "s3", "p", "q"} {
		id, _ := b.AddNode("Account")
		_ = b.SetProp(id, "name", n)
	}
	set := func(u, v chickpeas.NodeID, ct int64) {
		idx, err := b.AddRel(u, v, "transfer")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetRelPropAt(idx, "createTime", ct)
	}
	// s: (s)<-a[9]<-b[5] -- strictly decreasing, both o=a and o=b qualify.
	set(1, 0, 9)
	set(2, 1, 5)
	// s2: (s2)<-x[5]<-y[9] -- x qualifies (vacuous), y fails (5>9 false).
	set(4, 3, 5)
	set(5, 4, 9)
	// s3: (s3)<-p[5]<-q[5] -- p qualifies (vacuous), q fails (5>5 not strict).
	set(7, 6, 5)
	set(8, 7, 5)
	return b.Finalize("name")
}

// TestCrossSegmentMonoDropCorrectness verifies the engine result equals the
// pure all()/range filter semantics on monotonic, non-monotonic, and
// vacuous-length trails -- the evidence that dropping the redundant filter
// guard after the pushdown preserves results. TestMonoPushdownFires proves
// the pushdown (and thus the walk pruning) is actually engaged.
func TestCrossSegmentMonoDropCorrectness(t *testing.T) {
	g := monoTrailGraph(t)
	run := func(start string) map[string]bool {
		q := "MATCH (s:Account {name: '" + start + "'}) " +
			"MATCH p = TRAIL (s)<-[:transfer]-{1,3}(o:Account) " +
			"LET ts = [r IN rels(p) | r.createTime] " +
			"FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) " +
			"RETURN o.name AS oname, min(size(ts)) AS dist"
		rows, err := Run(g, q)
		if err != nil {
			t.Fatalf("%s: %v", start, err)
		}
		out := map[string]bool{}
		for {
			r, ok := rows.Next()
			if !ok {
				break
			}
			v, _ := r.Get("oname")
			s, _ := v.AsStr()
			out[s] = true
		}
		return out
	}
	cases := []struct {
		start string
		want  []string
	}{
		{"s", []string{"a", "b"}}, // both hops strictly decreasing
		{"s2", []string{"x"}},     // y fails 5>9
		{"s3", []string{"p"}},     // q fails 5>5 (not strict)
	}
	for _, c := range cases {
		got := run(c.start)
		if len(got) != len(c.want) {
			t.Fatalf("start=%s got %v, want %v", c.start, got, c.want)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Fatalf("start=%s missing %s (got %v)", c.start, w, got)
			}
		}
	}
}

// monoUnsetGraph builds decreasing-trail probes where some hops' keys are
// unset -- the cases where the walk pruning and the filter semantics
// historically diverged. sparse=true pads the createTime column below the
// dense threshold so unset positions read as absent (Null); sparse=false
// stays dense, where the engine's convention reads unset i64 as 0.
func monoUnsetGraph(t *testing.T, sparse bool) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(10, 11)
	for _, n := range []string{"s4", "m", "n", "s5", "u", "v", "w", "s6", "g1", "g2"} {
		id, _ := b.AddNode("Account")
		_ = b.SetProp(id, "name", n)
	}
	set := func(u, v chickpeas.NodeID, ct int64, withKey bool) {
		idx, err := b.AddRel(u, v, "transfer")
		if err != nil {
			t.Fatal(err)
		}
		if withKey {
			_ = b.SetRelPropAt(idx, "createTime", ct)
		}
	}
	// s4: (s4)<-m[unset]<-n[7] -- the first hop's key is unset.
	set(1, 0, 0, false)
	set(2, 1, 7, true)
	// s5: (s5)<-u[9]<-v[unset]<-w[3] -- an unset key between ordered keys.
	set(4, 3, 9, true)
	set(5, 4, 0, false)
	set(6, 5, 3, true)
	// s6: (s6)<-g1[3]<-g2[9] -- a genuine order violation.
	set(8, 7, 3, true)
	set(9, 8, 9, true)
	if sparse {
		// Keyless pad rels (never matched: different type) push the
		// createTime column's fill under the 80% dense threshold, so the
		// unset positions above read as absent rather than dense-zero.
		for range 4 {
			if _, err := b.AddRel(9, 8, "pad"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize("name")
}

// runMonoQuery runs a decreasing-trail query with the given FILTER conjunct
// and returns the reached endpoint names.
func runMonoQuery(t *testing.T, g *chickpeas.Snapshot, start, quant, filter string) map[string]bool {
	t.Helper()
	q := "MATCH (s:Account {name: '" + start + "'}) " +
		"MATCH p = TRAIL (s)<-[:transfer]-" + quant + "(o:Account) " +
		"LET ts = [r IN rels(p) | r.createTime] " +
		"FILTER " + filter + " " +
		"RETURN DISTINCT o.name AS oname"
	rows, err := Run(g, q)
	if err != nil {
		t.Fatalf("%s: %v", start, err)
	}
	out := map[string]bool{}
	for {
		r, ok := rows.Next()
		if !ok {
			break
		}
		v, _ := r.Get("oname")
		s, _ := v.AsStr()
		out[s] = true
	}
	return out
}

const allDescFilter = "all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1])"
const violationDescFilter = "size([i IN range(1, size(ts)) WHERE ts[i - 1] <= ts[i]]) = 0"

// TestMonoSparseKeyMatchesFilter pins the missing-key semantics on a sparse
// column: a 1-hop trail whose key is absent has no comparison to fail
// (all() over an empty range), so it must be kept; any longer trail through
// the absent key compares null and must be dropped for the all() form.
func TestMonoSparseKeyMatchesFilter(t *testing.T) {
	g := monoUnsetGraph(t, true)
	got := runMonoQuery(t, g, "s4", "{1,3}", allDescFilter)
	want := map[string]bool{"m": true}
	if len(got) != len(want) || !got["m"] {
		t.Fatalf("all() over sparse key: got %v, want %v", got, want)
	}
}

// TestMonoViolationCountNullsPass pins the violation-count form's null
// semantics on a sparse column: a null comparison is not a violation, so
// trails spanning an absent key are kept -- unlike the all() form.
func TestMonoViolationCountNullsPass(t *testing.T) {
	g := monoUnsetGraph(t, true)
	cases := []struct {
		start string
		want  []string
	}{
		// [9], [9,null], [9,null,3]: no pair compares true as a violation.
		{"s5", []string{"u", "v", "w"}},
		// [3], [3,9]: 3<=9 is a real violation -- g2 drops.
		{"s6", []string{"g1"}},
		// The same sparse chain the all() form prunes to {m}: the
		// violation form keeps both ([null] and [null,7] have no
		// comparable violating pair).
		{"s4", []string{"m", "n"}},
	}
	for _, c := range cases {
		got := runMonoQuery(t, g, c.start, "{1,3}", violationDescFilter)
		if len(got) != len(c.want) {
			t.Fatalf("start=%s got %v, want %v", c.start, got, c.want)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Fatalf("start=%s missing %s (got %v)", c.start, w, got)
			}
		}
	}
}

// TestMonoDenseUnsetZeroSemantics pins the walk-equals-filter equivalence
// on a DENSE column with unset positions: dense i64 storage cannot
// represent missing, so both the filter's list elements and the walk's key
// reads see 0 (the engine convention; task 041 tracks representing
// missingness in dense columns -- update these expectations if that
// changes).
func TestMonoDenseUnsetZeroSemantics(t *testing.T) {
	g := monoUnsetGraph(t, false)
	// s5 violation form: ts=[9,0,3] -- (9,0) is no violation (descending),
	// (0,3) is -- so w drops, matching the filter over the same 0 reads.
	got := runMonoQuery(t, g, "s5", "{1,3}", violationDescFilter)
	if len(got) != 2 || !got["u"] || !got["v"] {
		t.Fatalf("dense-unset violation form: got %v, want {u, v}", got)
	}
	// s4 all() form: [0] vacuous keeps m; [0,7] fails 0>7.
	got = runMonoQuery(t, g, "s4", "{1,3}", allDescFilter)
	if len(got) != 1 || !got["m"] {
		t.Fatalf("dense-unset all() form: got %v, want {m}", got)
	}
}

// TestMonoFloatKeyMatchesFilter pins the non-int key semantics: the walk
// compares with the filter's own three-valued Compare, so float keys prune
// exactly like the plain filter instead of emptying the result.
func TestMonoFloatKeyMatchesFilter(t *testing.T) {
	b := chickpeas.NewBuilder(4, 3)
	for _, n := range []string{"sf", "fa", "fb", "fc"} {
		id, _ := b.AddNode("Account")
		_ = b.SetProp(id, "name", n)
	}
	set := func(u, v chickpeas.NodeID, amt float64) {
		idx, err := b.AddRel(u, v, "transfer")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetRelPropAt(idx, "amount", amt)
	}
	// (sf)<-fa[9.5]<-fb[5.25]<-fc[7.5]: strictly decreasing through fb,
	// broken at fc.
	set(1, 0, 9.5)
	set(2, 1, 5.25)
	set(3, 2, 7.5)
	g := b.Finalize("name")
	q := "MATCH (s:Account {name: 'sf'}) " +
		"MATCH p = TRAIL (s)<-[:transfer]-{1,3}(o:Account) " +
		"LET xs = [r IN rels(p) | r.amount] " +
		"FILTER all(i IN range(0, size(xs) - 2) WHERE xs[i] > xs[i + 1]) " +
		"RETURN DISTINCT o.name AS oname"
	rows, err := Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for {
		r, ok := rows.Next()
		if !ok {
			break
		}
		v, _ := r.Get("oname")
		s, _ := v.AsStr()
		got[s] = true
	}
	if len(got) != 2 || !got["fa"] || !got["fb"] {
		t.Fatalf("float key: got %v, want {fa, fb}", got)
	}
}

// TestMinZeroNamedPathRejected pins the named-path guard: a zero-length or
// unbounded quantifier resolves a reachable set with no per-path rel lists,
// so binding a path over it must be a clean plan error (it previously
// crashed the executor on the unfillable rels slot). This also closes the
// min-0 monotonic hole end to end: the derived mono filter needs rels(p),
// which cannot exist for a min-0 quantifier, and the planner independently
// refuses to attach a MonoHopSpec to a min-0 var-expand.
func TestMinZeroNamedPathRejected(t *testing.T) {
	g := monoTrailGraph(t)
	for _, q := range []string{
		"MATCH p = (s:Account {name: 's'})<-[:transfer]-*(o:Account) RETURN o.name AS oname",
		"MATCH p = TRAIL (s:Account {name: 's'})<-[:transfer]-{0,3}(o:Account) RETURN o.name AS oname",
	} {
		if _, err := Run(g, q); err == nil || !strings.Contains(err.Error(), "named path") {
			t.Fatalf("want the named-path-over-reachability rejection, got err=%v for %s", err, q)
		}
	}
	// The unnamed form still runs as a reachability set.
	rows, err := Run(g, "MATCH (s:Account {name: 's'})<-[:transfer]-{0,3}(o:Account) RETURN o.name AS oname")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, ok := rows.Next(); !ok {
			break
		}
		n++
	}
	if n != 3 { // s (zero-length), a, b
		t.Fatalf("unnamed {0,3} reach rows = %d, want 3", n)
	}
}

// TestMonoOptionalWalkNotPruned pins the OPTIONAL interaction end to end:
// the boundary FILTER sits outside the optional match, so pruning the
// optional walk would fail the match and null-extend a row the filter
// (consumed by the push) could no longer drop. The planner must keep the
// conjunct as a post filter instead.
func TestMonoOptionalWalkNotPruned(t *testing.T) {
	g := monoTrailGraph(t)
	// From s2 with {2,3} the only trail is ts=[5,9], which violates the
	// descending order: the OPTIONAL matches, so no null extension, and
	// the filter drops the row -- zero rows. (A pruned walk would have
	// emitted one row with a null endpoint.)
	q := "MATCH (s:Account {name: 's2'}) " +
		"OPTIONAL MATCH p = TRAIL (s)<-[:transfer]-{2,3}(o:Account) " +
		"LET ts = [r IN rels(p) | r.createTime] " +
		"FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) " +
		"RETURN o.name AS oname"
	rows, err := Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, ok := rows.Next(); !ok {
			break
		}
		n++
	}
	if n != 0 {
		t.Fatalf("optional walk pruned: got %d rows, want 0 (the filter drops the matched trail)", n)
	}
}

// TestMonoPushdownFires asserts the monotonic pushdown reaches the bounded
// var-expand for the parsed CR1-shaped query, so the correctness test above
// genuinely exercises the walk pruning rather than a plain filter.
func TestMonoPushdownFires(t *testing.T) {
	g := monoTrailGraph(t)
	q := "MATCH (s:Account {name: 's'}) " +
		"MATCH p = TRAIL (s)<-[:transfer]-{1,3}(o:Account) " +
		"LET ts = [r IN rels(p) | r.createTime] " +
		"FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) " +
		"RETURN o.name AS oname, min(size(ts)) AS dist"
	q2, err := parseDesugar(q)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q2, graph.New(g))
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	for _, segs := range p.Branches {
		for _, s := range segs {
			for _, st := range s.Stages {
				ms, ok := st.(*plan.MatchStage)
				if !ok {
					continue
				}
				for i := range ms.Ops {
					if ms.Ops[i].MonoHop != nil {
						fired = true
					}
				}
			}
		}
	}
	if !fired {
		t.Fatal("monotonic pushdown did not reach the var-expand for the parsed CR1 shape")
	}
}
