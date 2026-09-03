// Matcher-memo contract tests: pointer-identity reuse for shareable
// matchers, no sharing for mutable ones, value-keyed properties, and
// foreign-snapshot bypass.
package graph

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestMatcherMemo pins the memo contract: shareable node matchers and
// all rel matchers reuse by pointer identity; a set-backed (mutable)
// node matcher never stores; a foreign snapshot bypasses.
func TestMatcherMemo(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	n0, _ := b.AddNode("A")
	_ = b.SetProp(n0, "name", "x")
	n1, _ := b.AddNode("A")
	_, _ = b.AddRel(n0, n1, "T")
	g := New(b.Finalize("memo"))

	mm := &MatcherMemo{}
	r1 := mm.CompileRelMatcher(g, []string{"T"})
	r2 := mm.CompileRelMatcher(g, []string{"T"})
	if r1 != r2 {
		t.Fatal("rel matcher did not memoize")
	}
	if r3 := mm.CompileRelMatcher(g, []string{"U"}); r3 == r1 {
		t.Fatal("distinct type list shared a matcher")
	}

	// Node matcher: with the label's dense bitmap unbuilt this is
	// set-backed (mutable) and must NOT store...
	m1 := mm.CompileNodeMatcher(g, []string{"A"}, nil)
	m2 := mm.CompileNodeMatcher(g, []string{"A"}, nil)
	if m1.Shareable() {
		if m1 != m2 {
			t.Fatal("shareable node matcher did not memoize")
		}
	} else if m1 == m2 {
		t.Fatal("mutable node matcher was shared")
	}
	// ...and after the snapshot densifies the label, compiles become
	// shareable and memoize.
	g.g.LabelDenseForced("A")
	m3 := mm.CompileNodeMatcher(g, []string{"A"}, nil)
	if !m3.Shareable() {
		t.Fatal("dense-backed matcher not shareable")
	}
	if m4 := mm.CompileNodeMatcher(g, []string{"A"}, nil); m4 != m3 {
		t.Fatal("dense-backed matcher did not memoize")
	}
	// Distinct property VALUES key distinct matchers.
	p1 := mm.CompileNodeMatcher(g, []string{"A"}, []PropSpec{{Key: "name", Val: value.Str("x")}})
	p2 := mm.CompileNodeMatcher(g, []string{"A"}, []PropSpec{{Key: "name", Val: value.Str("y")}})
	if p1 == p2 {
		t.Fatal("distinct property values shared a matcher")
	}
	// A second snapshot bypasses the adopted memo.
	b2 := chickpeas.NewBuilder(2, 2)
	_, _ = b2.AddNode("A")
	g2 := New(b2.Finalize("other"))
	g2.g.LabelDenseForced("A")
	o1 := mm.CompileNodeMatcher(g2, []string{"A"}, nil)
	o2 := mm.CompileNodeMatcher(g2, []string{"A"}, nil)
	if o1 == o2 {
		t.Fatal("foreign snapshot was served from the adopted memo")
	}
	// Nil memo compiles fresh.
	var nilMM *MatcherMemo
	if nm := nilMM.CompileRelMatcher(g, []string{"T"}); nm == nil {
		t.Fatal("nil memo returned nil")
	}
}
