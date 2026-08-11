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

// TestFingerprintSeesInlinePatternWhere pins that the cache key encodes
// the inline pattern predicates (ragedb's 296 rule): desugar clears these
// fields before the cache fingerprints today, so the key is currently
// order-dependent-correct -- this test makes it correct regardless of
// pipeline order, since two queries differing only in an inline WHERE
// must never share a plan.
func TestFingerprintSeesInlinePatternWhere(t *testing.T) {
	gt := &ast.Binary{Op: ast.OpGt, LHS: &ast.Prop{Var: "r", Key: "w"}, RHS: &ast.Lit{Value: ast.IntLit(1)}}
	mk := func(relWhere, nodeWhere ast.Expr) *ast.Query {
		return &ast.Query{Parts: []ast.QueryPart{{
			Clauses: []ast.Clause{&ast.Match{Patterns: []ast.Pattern{{
				Start: ast.NodePat{Var: "a", Where: nodeWhere},
				Hops: []ast.PatternHop{{
					Rel:  ast.RelPat{Var: "r", Dir: ast.DirOut, Types: []string{"R"}, Where: relWhere},
					Node: ast.NodePat{Var: "b"},
				}},
			}}}},
			Ret: ast.Projection{Items: []ast.ReturnItem{{Expr: &ast.Var{Name: "a"}, Alias: "a"}}},
		}}}
	}
	plain := ast.Fingerprint(mk(nil, nil))
	if withRel := ast.Fingerprint(mk(gt, nil)); withRel == plain {
		t.Fatal("rel inline WHERE dropped from the fingerprint: filtered and unfiltered queries share a cache key")
	}
	if withNode := ast.Fingerprint(mk(nil, gt)); withNode == plain {
		t.Fatal("node inline WHERE dropped from the fingerprint")
	}
}
