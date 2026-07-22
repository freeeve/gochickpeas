package exec

import (
	"testing"

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
