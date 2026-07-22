package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
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
