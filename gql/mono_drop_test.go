package gql

import (
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
