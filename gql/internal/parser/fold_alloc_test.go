// Allocation guard for case-insensitive keyword dispatch (task 147). The
// parser folds every identifier token to compare it against reserved words
// and keywords; doing that with strings.ToLower allocated a lowercased copy
// of each mixed-case identifier (the single largest query-path allocation
// site in the BI Q1 profile). foldLower folds into a stack buffer instead.
package parser

import (
	"strings"
	"testing"
)

// TestIdentifierFoldIsAllocationFree is a differential: the same query with
// lowercase vs mixed-case identifiers must parse with the SAME allocation
// count. Structure and token count are identical, so the only thing that can
// differ is a per-identifier lowercased-copy allocation -- which the stack
// fold eliminates. Reverting foldLower to strings.ToLower makes the
// mixed-case form allocate once per camelCase identifier and reddens this.
func TestIdentifierFoldIsAllocationFree(t *testing.T) {
	const lower = "match (nodevar:label) where nodevar.propname < 5 and nodevar.other is not null " +
		"return nodevar.propname as col order by col desc"
	const mixed = "MATCH (nodeVar:Label) WHERE nodeVar.propName < 5 AND nodeVar.other IS NOT NULL " +
		"RETURN nodeVar.propName AS col ORDER BY col DESC"
	if _, err := Parse(lower); err != nil {
		t.Fatalf("parse lower: %v", err)
	}
	if _, err := Parse(mixed); err != nil {
		t.Fatalf("parse mixed: %v", err)
	}
	al := testing.AllocsPerRun(300, func() {
		if _, err := Parse(lower); err != nil {
			t.Fatal(err)
		}
	})
	am := testing.AllocsPerRun(300, func() {
		if _, err := Parse(mixed); err != nil {
			t.Fatal(err)
		}
	})
	// The mixed-case identifiers (nodeVar, propName) appear many times; a
	// ToLower per occurrence would push am well above al. Folding keeps them
	// equal -- allow 1 alloc of slack for measurement noise only.
	if am > al+1 {
		t.Fatalf("mixed-case parse allocs=%.0f vs lowercase=%.0f: identifier folding allocates per identifier (want within 1)", am, al)
	}
}

// TestFoldLowerMatchesToLower pins the fold's semantics against the
// strings.ToLower it replaced for every ASCII keyword-length input.
func TestFoldLowerMatchesToLower(t *testing.T) {
	for _, s := range []string{"", "a", "MATCH", "Match", "creationDate", "ZONED_DATETIME", "x", "ORDER"} {
		var buf [24]byte
		folded, ok := foldLower(s, buf[:])
		if !ok {
			t.Fatalf("foldLower(%q) did not fit a 24-byte buffer", s)
		}
		if got, want := string(folded), strings.ToLower(s); got != want {
			t.Fatalf("foldLower(%q) = %q, want %q", s, got, want)
		}
	}
	// An over-long identifier folds to ok=false -- it can equal no keyword.
	var buf [8]byte
	if _, ok := foldLower("aaaaaaaaaaaa", buf[:]); ok {
		t.Fatal("foldLower fit an over-long identifier into an 8-byte buffer")
	}
}
