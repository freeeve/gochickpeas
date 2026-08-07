// Rename-invariance sweep for substGroupKeys (task 238): the cross
// product of every by-name reference form and every wrapper context,
// asserting per cell that a renamed grouping key either follows the
// rename completely or survives exactly where the documented policy says
// it must. The matrix subsumes the single-position kind cases and was
// mutation-verified: reverting each of the three walker fixes (label-test
// arm, binder-body descent, colliding-local alpha-rename) in turn fails
// the sweep.
package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// sweepExpect classifies a wrapper context's policy for a renamed key
// referenced inside it.
type sweepExpect int

const (
	// sweepRewrites: every reference follows the key to its column.
	sweepRewrites sweepExpect = iota
	// sweepKeeps: the reference survives free (correlated subquery
	// interiors -- their filters evaluate against the subquery scope).
	sweepKeeps
	// sweepShadows: the binder rebinds the key's source name; the body
	// reference means the local and must stay untouched (no column
	// reference may appear).
	sweepShadows
	// sweepCollides: the binder's local equals the key's output name; the
	// local is alpha-renamed away and the reference then rewrites.
	sweepCollides
)

// sweepRefForms are the by-name reference forms -- the shapes a walker
// arm can miss individually.
func sweepRefForms() map[string]func(v string) ast.Expr {
	return map[string]func(v string) ast.Expr{
		"var":  func(v string) ast.Expr { return &ast.Var{Name: v} },
		"prop": func(v string) ast.Expr { return &ast.Prop{Var: v, Key: "k"} },
		"label": func(v string) ast.Expr {
			return &ast.HasLabelExpr{Var: v, Expr: &ast.LabelExpr{Name: "L"}}
		},
		"mapproj": func(v string) ast.Expr {
			return &ast.MapProj{Var: v, Entries: []ast.MapProjEntry{{Kind: ast.MapProjProp, Key: "k"}}}
		},
	}
}

// sweepContexts are the wrapper contexts, each embedding the reference at
// one position of one construct (plus a few deep compositions).
func sweepContexts() []struct {
	name   string
	wrap   func(ref ast.Expr) ast.Expr
	expect sweepExpect
} {
	one := func() ast.Expr { return &ast.Lit{Value: ast.IntLit(1)} }
	list := func() ast.Expr { return &ast.ListExpr{Elems: []ast.Expr{one()}} }
	pat := func() *ast.Pattern { return &ast.Pattern{Start: ast.NodePat{Var: "z"}} }
	w := func(name string, expect sweepExpect, wrap func(ast.Expr) ast.Expr) struct {
		name   string
		wrap   func(ref ast.Expr) ast.Expr
		expect sweepExpect
	} {
		return struct {
			name   string
			wrap   func(ref ast.Expr) ast.Expr
			expect sweepExpect
		}{name, wrap, expect}
	}
	return []struct {
		name   string
		wrap   func(ref ast.Expr) ast.Expr
		expect sweepExpect
	}{
		w("top", sweepRewrites, func(r ast.Expr) ast.Expr { return r }),
		w("unary", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Unary{Op: ast.Not, Expr: r} }),
		w("isnull", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.IsNull{Expr: r} }),
		w("istruth", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.IsTruth{Expr: r} }),
		w("istyped", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.IsTyped{Expr: r} }),
		w("binary-lhs", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Binary{Op: ast.OpAdd, LHS: r, RHS: one()} }),
		w("binary-rhs", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Binary{Op: ast.OpAdd, LHS: one(), RHS: r} }),
		w("func-arg", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Func{Name: "abs", Args: []ast.Expr{r}} }),
		w("listexpr", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.ListExpr{Elems: []ast.Expr{r}} }),
		w("in-expr", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.In{Expr: r, List: list()} }),
		w("in-list", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.In{Expr: one(), List: r} }),
		w("case-operand", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Case{Operand: r, Whens: []ast.CaseWhen{{Cond: one(), Result: one()}}}
		}),
		w("case-cond", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Case{Whens: []ast.CaseWhen{{Cond: r, Result: one()}}}
		}),
		w("case-result", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Case{Whens: []ast.CaseWhen{{Cond: one(), Result: r}}}
		}),
		w("case-else", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Case{Whens: []ast.CaseWhen{{Cond: one(), Result: one()}}, Else: r}
		}),
		w("index-base", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Index{Base: r, Idx: one()} }),
		w("index-idx", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Index{Base: list(), Idx: r} }),
		w("slice-from", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.Slice{Base: list(), From: r, To: one()} }),
		w("propof-base", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.PropOf{Base: r, Key: "k"} }),
		w("maplit-val", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: r}}}
		}),
		w("mapproj-field", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.MapProj{Var: "m", Entries: []ast.MapProjEntry{{Kind: ast.MapProjField, Key: "f", Expr: r}}}
		}),
		w("listpred-list", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListPred{Quant: ast.QuantAll, Var: "v", List: r, Pred: one()}
		}),
		w("listpred-pred", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListPred{Quant: ast.QuantAll, Var: "v", List: list(), Pred: r}
		}),
		w("listcomp-list", sweepRewrites, func(r ast.Expr) ast.Expr { return &ast.ListComp{Var: "v", List: r} }),
		w("listcomp-filter", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListComp{Var: "v", List: list(), Filter: r}
		}),
		w("listcomp-map", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListComp{Var: "v", List: list(), Map: r}
		}),
		w("reduce-init", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Reduce{Acc: "a", Init: r, Var: "v", List: list(), Body: one()}
		}),
		w("reduce-body", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Reduce{Acc: "a", Init: one(), Var: "v", List: list(), Body: r}
		}),
		// Deep compositions: the fixed bugs lived exactly one level down.
		w("case-in-listcomp-map", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListComp{Var: "v", List: list(),
				Map: &ast.Case{Whens: []ast.CaseWhen{{Cond: r, Result: one()}}}}
		}),
		w("listcomp-in-reduce-body", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.Reduce{Acc: "a", Init: one(), Var: "v", List: list(),
				Body: &ast.ListComp{Var: "w", List: list(), Map: r}}
		}),
		w("func-in-listpred-pred", sweepRewrites, func(r ast.Expr) ast.Expr {
			return &ast.ListPred{Quant: ast.QuantAll, Var: "v", List: list(),
				Pred: &ast.Func{Name: "abs", Args: []ast.Expr{r}}}
		}),
		// Correlated subquery interiors: policy keeps the reference.
		w("exists-where", sweepKeeps, func(r ast.Expr) ast.Expr { return &ast.Exists{Pattern: pat(), Where: r} }),
		w("countsub-where", sweepKeeps, func(r ast.Expr) ast.Expr { return &ast.CountSub{Pattern: pat(), Where: r} }),
		w("patterncomp-proj", sweepKeeps, func(r ast.Expr) ast.Expr {
			return &ast.PatternComp{Pattern: pat(), Proj: r}
		}),
		// Binder rebinds the source name: the reference means the local.
		w("shadow-listcomp-map", sweepShadows, func(r ast.Expr) ast.Expr {
			return &ast.ListComp{Var: "n", List: list(), Map: r}
		}),
		w("shadow-reduce-body", sweepShadows, func(r ast.Expr) ast.Expr {
			return &ast.Reduce{Acc: "n", Init: one(), Var: "v", List: list(), Body: r}
		}),
		// Binder local equals the output name: alpha-rename, then rewrite.
		w("collide-listcomp-map", sweepCollides, func(r ast.Expr) ast.Expr {
			return &ast.ListComp{Var: "m", List: list(), Map: r}
		}),
		w("collide-listpred-pred", sweepCollides, func(r ast.Expr) ast.Expr {
			return &ast.ListPred{Quant: ast.QuantAll, Var: "m", List: list(), Pred: r}
		}),
	}
}

// TestSubstGroupKeysSweep runs the ref-form x wrapper-context matrix.
func TestSubstGroupKeysSweep(t *testing.T) {
	for _, ctx := range sweepContexts() {
		for formName, form := range sweepRefForms() {
			name := ctx.name + "/" + formName
			groups := []groupCol{{idx: 0, name: "m", expr: &ast.Var{Name: "n"}}}
			got := substGroupKeys(ctx.wrap(form("n")), groups)
			free := freeVarsOutside(got, []string{"m"})
			switch ctx.expect {
			case sweepRewrites, sweepCollides:
				if len(free) != 0 {
					t.Errorf("%s: free = %v, want none", name, free)
				}
				if MentionsVar(got, "n") && ctx.expect == sweepRewrites {
					t.Errorf("%s: residual reference to n", name)
				}
			case sweepKeeps:
				if !slices.Equal(free, []string{"n"}) {
					t.Errorf("%s: free = %v, want [n] (policy keep)", name, free)
				}
			case sweepShadows:
				if len(free) != 0 {
					t.Errorf("%s: free = %v, want none (local-bound)", name, free)
				}
				if MentionsVar(got, "m") {
					t.Errorf("%s: column reference introduced under a shadowing binder", name)
				}
			}
		}
	}
}
