package ast_test

import (
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/ast/asttest"
)

// TestFingerprintCoversEveryExprKind is the fingerprint walk's roll call,
// enforced behaviorally (ragedb's 293 lesson: a cache-key walk's missed
// arm ENABLES a wrong answer -- two different queries collapse to one key
// and serve each other's plans). Every ast.Expr kind must fingerprint
// without reaching the fail-closed default (whose NUL-prefixed poison
// marker cannot occur in legitimate output), the per-kind fingerprints
// must be pairwise distinct, and fingerprinting must be deterministic.
// A future kind added without an fpExpr arm fails here.
func TestFingerprintCoversEveryExprKind(t *testing.T) {
	wrap := func(e ast.Expr) *ast.Query {
		return &ast.Query{Parts: []ast.QueryPart{{
			Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: e, Alias: "x"}}},
		}}}
	}
	seen := map[string]string{}
	for _, c := range asttest.KindCases("n") {
		fp := ast.Fingerprint(wrap(c.Build()))
		if strings.Contains(fp, "\x00") {
			t.Errorf("ast.%s reaches the fingerprint default (poison marker in %q): add an fpExpr arm", c.Kind, fp)
			continue
		}
		if fp2 := ast.Fingerprint(wrap(c.Build())); fp2 != fp {
			t.Errorf("ast.%s fingerprints nondeterministically: %q vs %q", c.Kind, fp, fp2)
		}
		if prev, dup := seen[fp]; dup {
			t.Errorf("ast.%s and ast.%s share a fingerprint %q -- the cache would conflate them", c.Kind, prev, fp)
		}
		seen[fp] = c.Kind
	}
	if len(seen) == 0 {
		t.Fatal("no kinds fingerprinted -- the roll call is not enforcing anything")
	}
}
