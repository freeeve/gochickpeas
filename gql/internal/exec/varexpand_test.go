package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestHopCarryAllows covers the monotonic per-hop constraint: an ascending
// gate admits a strictly larger key, a descending gate a strictly smaller
// one, equal keys are rejected either way, and incomparable keys fall back
// to the nullsPass policy.
func TestHopCarryAllows(t *testing.T) {
	asc := &hopCarry{ascending: true}
	if !asc.allows(value.Int(1), value.Int(2)) {
		t.Fatal("ascending admits 1 -> 2")
	}
	if asc.allows(value.Int(2), value.Int(1)) {
		t.Fatal("ascending rejects 2 -> 1")
	}
	if asc.allows(value.Int(1), value.Int(1)) {
		t.Fatal("ascending rejects an equal key")
	}

	desc := &hopCarry{ascending: false}
	if !desc.allows(value.Int(2), value.Int(1)) {
		t.Fatal("descending admits 2 -> 1")
	}
	if desc.allows(value.Int(1), value.Int(2)) {
		t.Fatal("descending rejects 1 -> 2")
	}
	if desc.allows(value.Int(1), value.Int(1)) {
		t.Fatal("descending rejects an equal key")
	}

	// Incomparable keys (different kinds) follow nullsPass.
	pass := &hopCarry{ascending: true, nullsPass: true}
	drop := &hopCarry{ascending: true, nullsPass: false}
	if !pass.allows(value.Int(1), value.Str("x")) {
		t.Fatal("incomparable + nullsPass must allow")
	}
	if drop.allows(value.Int(1), value.Str("x")) {
		t.Fatal("incomparable + !nullsPass must reject")
	}
}

// posKeyEval is a hop-gate RowEval whose key for a hop is its rel position,
// so a walk's keys are exactly the positions it steps through.
type posKeyEval struct{}

func (posKeyEval) Eval(_ *eval.Ctx, row []value.Value, _ map[string]int) value.Value {
	pos, _ := row[0].AsRel()
	return value.Int(int64(pos))
}

// posEvenEval is a hop-filter RowEval that accepts an even rel position.
type posEvenEval struct{}

func (posEvenEval) Eval(_ *eval.Ctx, row []value.Value, _ map[string]int) value.Value {
	pos, _ := row[0].AsRel()
	return value.Bool(pos%2 == 0)
}

// TestHopFilterKeep covers the per-hop predicate: keep evaluates the filter
// against the candidate relationship and returns its truthiness.
func TestHopFilterKeep(t *testing.T) {
	var ctx *eval.Ctx // posEvenEval ignores ctx
	h := &hopFilter{eval: posEvenEval{}}
	if !h.keep(ctx, 4) {
		t.Fatal("an even rel position must pass the filter")
	}
	if h.keep(ctx, 3) {
		t.Fatal("an odd rel position must fail the filter")
	}
}

// TestHopCarryStep covers the stateful per-hop constraint: the first hop
// always steps and seeds the state, an ascending gate admits a strictly
// larger key and rejects a smaller one (leaving the prior state), and a
// descending gate is the mirror.
func TestHopCarryStep(t *testing.T) {
	var ctx *eval.Ctx // posKeyEval ignores ctx

	asc := &hopCarry{eval: posKeyEval{}, ascending: true}
	st, ok := asc.step(ctx, 3, carryState{})
	if !ok || !st.have {
		t.Fatal("the first hop always steps and seeds state")
	}
	if v, _ := st.val.AsInt(); v != 3 {
		t.Fatalf("first hop key = %d, want 3", v)
	}
	up, ok := asc.step(ctx, 5, st)
	if !ok {
		t.Fatal("ascending admits 3 -> 5")
	}
	if v, _ := up.val.AsInt(); v != 5 {
		t.Fatalf("stepped key = %d, want 5", v)
	}
	down, ok := asc.step(ctx, 4, up)
	if ok {
		t.Fatal("ascending rejects 5 -> 4")
	}
	if v, _ := down.val.AsInt(); v != 5 {
		t.Fatal("a rejected step returns the prior state unchanged")
	}

	desc := &hopCarry{eval: posKeyEval{}, ascending: false}
	sd, _ := desc.step(ctx, 9, carryState{})
	if _, ok := desc.step(ctx, 4, sd); !ok {
		t.Fatal("descending admits 9 -> 4")
	}
	if _, ok := desc.step(ctx, 12, sd); ok {
		t.Fatal("descending rejects 9 -> 12")
	}
}

// TestVarReach covers the variable-length reachability walk (no hop gate,
// no rel-uniqueness): from a start node it collects nodes reachable over the
// rel type within [Min,Max] hops, includes the start when Min is 0, and
// excludes nodes below Min. Fixture: a directed chain 0 -R-> 1 -R-> 2 -R-> 3.
func TestVarReach(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := bld.AddRel(graph.NodeID(i), graph.NodeID(i+1), "R"); err != nil {
			t.Fatal(err)
		}
	}
	sg := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: sg}
	rm := sg.CompileRelMatcher([]string{"R"})
	m := sg.CompileNodeMatcher(nil, nil)

	u := func(x uint64) *uint64 { return &x }
	reach := func(min uint64, max *uint64) map[graph.NodeID]bool {
		op := &plan.BindOp{Kind: plan.OpVarExpand, Dir: graph.Outgoing, Types: []string{"R"}, Min: min, Max: max}
		var out []graph.NodeID
		var rs reachScratch
		varReach(ctx, 0, op, m, rm, hopGate{}, 0, false, &uniqEnv{}, &out, &rs)
		got := map[graph.NodeID]bool{}
		for _, n := range out {
			got[n] = true
		}
		return got
	}
	// {1,1}: exactly one hop from 0.
	if got := reach(1, u(1)); len(got) != 1 || !got[1] {
		t.Fatalf("{1,1} = %v, want {1}", got)
	}
	// {1,2}: one or two hops.
	if got := reach(1, u(2)); len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("{1,2} = %v, want {1,2}", got)
	}
	// {0,2}: Min 0 includes the start node.
	if got := reach(0, u(2)); len(got) != 3 || !got[0] || !got[1] || !got[2] {
		t.Fatalf("{0,2} = %v, want {0,1,2}", got)
	}
	// {2,3}: at least two hops excludes node 1.
	if got := reach(2, u(3)); len(got) != 2 || !got[2] || !got[3] {
		t.Fatalf("{2,3} = %v, want {2,3}", got)
	}
}

// TestVarExpandCandidates covers the var-length entry dispatcher over a
// functional chain 0-R->1-R->2-R->3: a zero-minimum unbounded quantifier
// routes through the deduped reach (the functional-chain fast path), a
// bounded quantifier through per-path enumeration; both yield the reachable
// set on a chain.
func TestVarExpandCandidates(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := bld.AddRel(graph.NodeID(i), graph.NodeID(i+1), "R"); err != nil {
			t.Fatal(err)
		}
	}
	sg := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: sg}
	rm := sg.CompileRelMatcher([]string{"R"})
	m := sg.CompileNodeMatcher(nil, nil)

	run := func(op *plan.BindOp) map[graph.NodeID]bool {
		row := make([]value.Value, 2)
		row[op.From] = value.Node(graph.NodeID(0))
		var cand []graph.NodeID
		var relData []uint32
		var relRanges [][2]int
		var pairData [][2]graph.NodeID
		var pairRanges [][2]int
		var scr genScratch
		varExpandCandidates(ctx, op, m, rm, hopGate{}, row, &uniqEnv{}, &cand, &relData, &relRanges, &pairData, &pairRanges, &scr)
		got := map[graph.NodeID]bool{}
		for _, n := range cand {
			got[n] = true
		}
		return got
	}
	u := func(x uint64) *uint64 { return &x }

	// Bounded {1,2}: one or two hops from 0 -> {1,2}.
	if got := run(&plan.BindOp{Kind: plan.OpVarExpand, From: 0, To: 1, Dir: graph.Outgoing, Types: []string{"R"}, Min: 1, Max: u(2)}); len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("{1,2} candidates = %v, want {1,2}", got)
	}
	// Zero-minimum unbounded {0,}: the whole reachable set incl. the start.
	if got := run(&plan.BindOp{Kind: plan.OpVarExpand, From: 0, To: 1, Dir: graph.Outgoing, Types: []string{"R"}, Min: 0, Max: nil}); len(got) != 4 || !got[0] || !got[3] {
		t.Fatalf("{0,} candidates = %v, want {0,1,2,3}", got)
	}
}
