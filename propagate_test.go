package chickpeas

import (
	"math"
	"testing"
)

// propGraph builds the little flow graph the PropagateBFS tests share:
//
//	s -[amt 5, ts 100]-> a -[amt 7]-> c
//	s -[amt 3, ts 200]-> b -[amt 2]-> c
//	s -[amt 0]-> d
//	b -[amt 4]-> s   (cycle back)
//
// Node index order: s, a, b, c, d.
func propGraph(t *testing.T) (*Snapshot, []NodeID) {
	t.Helper()
	b := NewBuilder(8, 16)
	ids := make([]NodeID, 5)
	for i := range ids {
		n, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = n
	}
	s, a, bb, c, d := ids[0], ids[1], ids[2], ids[3], ids[4]
	rel := func(u, v NodeID, amt float64, ts int64) {
		t.Helper()
		pos, err := b.AddRel(u, v, "flow")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(pos, "amt", amt); err != nil {
			t.Fatal(err)
		}
		if ts != 0 {
			if err := b.SetRelPropAt(pos, "ts", ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	rel(s, a, 5, 100)
	rel(s, bb, 3, 200)
	rel(a, c, 7, 0)
	rel(bb, c, 2, 0)
	rel(s, d, 0, 0)
	rel(bb, s, 4, 0)
	return b.Finalize(), ids
}

func propMap(rs []PropagateResult) map[NodeID]PropagateResult {
	m := make(map[NodeID]PropagateResult, len(rs))
	for _, r := range rs {
		m[r.Node] = r
	}
	return m
}

func baseOpts() PropagateOpts {
	return PropagateOpts{
		RelTypes:  []string{"flow"},
		Direction: Outgoing,
		MaxDepth:  3,
		ValueProp: "amt",
	}
}

// Ascending fan-out expands b (amt 3) before a (amt 5), so b's cheap edge
// claims c first; a's larger edge finds c already claimed.
func TestPropagateFirstClaimAsc(t *testing.T) {
	g, ids := propGraph(t)
	s, a, b, c := ids[0], ids[1], ids[2], ids[3]
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, baseOpts()))
	want := map[NodeID]PropagateResult{
		s: {s, 10, 1}, a: {a, 5, 2}, b: {b, 3, 2}, c: {c, 2, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Fatalf("node %d: got %+v want %+v", n, got[n], w)
		}
	}
}

// Descending fan-out expands a first, so its amt-7 edge claims c.
func TestPropagateFirstClaimDesc(t *testing.T) {
	g, ids := propGraph(t)
	s, c := ids[0], ids[3]
	opts := baseOpts()
	opts.Desc = true
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, opts))
	if r := got[c]; r.Value != 7 || r.Depth != 3 {
		t.Fatalf("c: got %+v", r)
	}
}

// Truncation cuts the ordered fan-out BEFORE the value gate, exactly like
// the FinBench truncation strategy: at limit 2 ascending, the seed keeps
// [d(0), b(3)] -- a(5) is cut entirely -- and d then falls to the gate, so
// c arrives via b. At limit 1 the kept edge is the gated d(0) and nothing
// propagates at all.
func TestPropagateTruncationCut(t *testing.T) {
	g, ids := propGraph(t)
	s, a, b, c, d := ids[0], ids[1], ids[2], ids[3], ids[4]
	opts := baseOpts()
	opts.TruncLimit = 2
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, opts))
	if _, reached := got[a]; reached {
		t.Fatal("a should be truncated away")
	}
	if _, reached := got[d]; reached {
		t.Fatal("d should be gated after truncation")
	}
	if got[b].Value != 3 || got[c].Value != 2 {
		t.Fatalf("got %v", got)
	}
	opts.TruncLimit = 1
	if got := g.PropagateBFS([]PropagateSeed{{s, 10}}, opts); len(got) != 1 {
		t.Fatalf("limit 1: got %v", got)
	}
}

// Two seeds merge: values sum, depth takes the minimum; seeding the same
// node twice runs twice and sums.
func TestPropagateMultiSeedMerge(t *testing.T) {
	g, ids := propGraph(t)
	s, a, c := ids[0], ids[1], ids[3]
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}, {a, 20}}, baseOpts()))
	if r := got[a]; r.Value != 25 || r.Depth != 1 {
		t.Fatalf("a: got %+v", r)
	}
	if r := got[c]; r.Value != 9 || r.Depth != 2 {
		t.Fatalf("c: got %+v", r)
	}
	got = propMap(g.PropagateBFS([]PropagateSeed{{s, 1}, {s, 2}}, baseOpts()))
	if r := got[s]; r.Value != 3 || r.Depth != 1 {
		t.Fatalf("s twice: got %+v", r)
	}
}

// MaxDepth 1 records seeds only; 2 stops one hop out; values below 1 mean
// seeds only too.
func TestPropagateDepthCap(t *testing.T) {
	g, ids := propGraph(t)
	s, c := ids[0], ids[3]
	opts := baseOpts()
	opts.MaxDepth = 1
	if got := g.PropagateBFS([]PropagateSeed{{s, 10}}, opts); len(got) != 1 || got[0].Node != s {
		t.Fatalf("depth 1: got %v", got)
	}
	opts.MaxDepth = 0
	if got := g.PropagateBFS([]PropagateSeed{{s, 10}}, opts); len(got) != 1 {
		t.Fatalf("depth 0: got %v", got)
	}
	opts.MaxDepth = 2
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, opts))
	if _, reached := got[c]; reached || len(got) != 3 {
		t.Fatalf("depth 2: got %v", got)
	}
}

// The default MinValue 0 gates out the amt-0 edge to d; -Inf admits it.
func TestPropagateMinValueGate(t *testing.T) {
	g, ids := propGraph(t)
	s, d := ids[0], ids[4]
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, baseOpts()))
	if _, reached := got[d]; reached {
		t.Fatal("d should be gated out at MinValue 0")
	}
	opts := baseOpts()
	opts.MinValue = math.Inf(-1)
	got = propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, opts))
	if r := got[d]; r.Value != 0 || r.Depth != 2 {
		t.Fatalf("d: got %+v", r)
	}
}

// FilterProp restricts eligibility to rels whose ts sits in the window;
// rels without the property are excluded while filtering.
func TestPropagateRelPropFilter(t *testing.T) {
	g, ids := propGraph(t)
	s, b := ids[0], ids[2]
	opts := baseOpts()
	opts.FilterProp = "ts"
	opts.FilterMin, opts.FilterMax = 150, 250
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, opts))
	// Only s->b (ts 200) is eligible; b's own rels carry no ts.
	if len(got) != 2 || got[b].Value != 3 {
		t.Fatalf("got %v", got)
	}
	// A filter over a missing column matches nothing beyond the seeds.
	opts.FilterProp = "nope"
	if got := g.PropagateBFS([]PropagateSeed{{s, 10}}, opts); len(got) != 1 {
		t.Fatalf("missing filter col: got %v", got)
	}
}

// A cycle back to the seed does not re-claim it within a run, and a
// missing value column propagates nothing at the default gate.
func TestPropagateCycleAndMissingValueProp(t *testing.T) {
	g, ids := propGraph(t)
	s := ids[0]
	got := propMap(g.PropagateBFS([]PropagateSeed{{s, 10}}, baseOpts()))
	if r := got[s]; r.Value != 10 || r.Depth != 1 {
		t.Fatalf("s: got %+v (cycle must not re-claim the seed)", r)
	}
	opts := baseOpts()
	opts.ValueProp = "nope"
	if got := g.PropagateBFS([]PropagateSeed{{s, 10}}, opts); len(got) != 1 {
		t.Fatalf("missing value col: got %v", got)
	}
}

// Incoming direction walks rels backwards: from a the s->a rel is the only
// incoming flow.
func TestPropagateIncoming(t *testing.T) {
	g, ids := propGraph(t)
	s, a := ids[0], ids[1]
	opts := baseOpts()
	opts.Direction = Incoming
	opts.MaxDepth = 2
	got := propMap(g.PropagateBFS([]PropagateSeed{{a, 1}}, opts))
	if r := got[s]; r.Value != 5 || r.Depth != 2 {
		t.Fatalf("s via incoming: got %v", got)
	}
}
