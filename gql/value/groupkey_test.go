// Key-encoding tier consistency: AppendKey (the DISTINCT/grouping key)
// versus Compare (the equality the language defines DISTINCT in terms
// of). The two agree everywhere except the exact-int regime above 2^53,
// where Compare's float64 coercion calls adjacent int64s equal while
// the key encoding keeps them apart -- two values the engine calls
// equal survive DISTINCT and form separate groups. Pinned as CURRENT
// behavior with the contradiction named (the cross-engine finding from
// the Rust sibling's key-tier census; no corpus exposure -- LDBC ids
// sit three orders below 2^53 -- but reachable with snowflake-style
// ids). Resolving it either way is a semantics choice: an exact
// equality breaks Int/Float transitivity, a lossy key changes group
// counts; whichever lands, these assertions invert deliberately.
package value

import (
	"bytes"
	"testing"
)

func TestKeyEncodingVsEqualityAbove2p53(t *testing.T) {
	a, b := Int(9007199254740992), Int(9007199254740993)
	if c, ok := Compare(a, b); !ok || c != 0 {
		t.Fatalf("Compare(2^53, 2^53+1) = (%d, %v); the float64-coerced equality calls them EQUAL today", c, ok)
	}
	ka := AppendKey(nil, a)
	kb := AppendKey(nil, b)
	if bytes.Equal(ka, kb) {
		t.Fatal("AppendKey(2^53) == AppendKey(2^53+1); the exact key tier keeps them DISTINCT today -- if this now merges, the contradiction was resolved toward the lossy side: update the test to assert agreement")
	}
}

// The key encoding DOES canonicalize across the Int/Float boundary --
// an integral float and its int share one key, agreeing with equality.
// The contradiction above is therefore only the exact-int regime, not
// kind unification.
func TestKeyEncodingUnifiesIntegralFloat(t *testing.T) {
	ka := AppendKey(nil, Int(5))
	kb := AppendKey(nil, Float(5.0))
	if !bytes.Equal(ka, kb) {
		t.Fatal("AppendKey(Int 5) != AppendKey(Float 5.0); DISTINCT would split what = unifies at small magnitudes")
	}
}
