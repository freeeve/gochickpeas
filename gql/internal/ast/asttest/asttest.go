// Package asttest is shared test scaffolding for the exhaustive
// expression walkers (substitution, alpha-rename, reference checking):
// one buildable instance of every ast.Expr kind referencing a chosen free
// variable, plus a roll call that fails when the ast package grows a kind
// no walker test has decided a policy for. Test-only by convention; never
// import it from production code.
package asttest

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// KindCase is one ast.Expr kind with a builder that returns a fresh
// instance referencing the free variable it was constructed with.
type KindCase struct {
	Kind  string
	Build func() ast.Expr
}

// KindCases returns one case per ast.Expr kind, each referencing ref as a
// free variable where the kind can hold one (Lit cannot). Binder kinds
// use locals named v/a/z, and Cost's second endpoint is named q.
func KindCases(ref string) []KindCase {
	n := func() ast.Expr { return &ast.Var{Name: ref} }
	one := func() ast.Expr { return &ast.Lit{Value: ast.IntLit(1)} }
	pat := func() *ast.Pattern { return &ast.Pattern{Start: ast.NodePat{Var: "z"}} }
	lbl := func() *ast.LabelExpr { return &ast.LabelExpr{Name: "L"} }
	return []KindCase{
		{"Lit", one},
		{"Var", n},
		{"Prop", func() ast.Expr { return &ast.Prop{Var: ref, Key: "x"} }},
		{"PropOf", func() ast.Expr { return &ast.PropOf{Base: n(), Key: "x"} }},
		{"HasLabelExpr", func() ast.Expr { return &ast.HasLabelExpr{Var: ref, Expr: lbl()} }},
		{"MapProj", func() ast.Expr {
			return &ast.MapProj{Var: ref, Entries: []ast.MapProjEntry{{Kind: ast.MapProjField, Key: "k", Expr: n()}}}
		}},
		{"Unary", func() ast.Expr { return &ast.Unary{Op: ast.Not, Expr: n()} }},
		{"IsNull", func() ast.Expr { return &ast.IsNull{Expr: n()} }},
		{"IsTruth", func() ast.Expr { return &ast.IsTruth{Expr: n()} }},
		{"IsTyped", func() ast.Expr { return &ast.IsTyped{Expr: n()} }},
		{"Binary", func() ast.Expr { return &ast.Binary{Op: ast.OpAdd, LHS: n(), RHS: one()} }},
		{"Func", func() ast.Expr { return &ast.Func{Name: "abs", Args: []ast.Expr{n()}} }},
		{"ListExpr", func() ast.Expr { return &ast.ListExpr{Elems: []ast.Expr{n()}} }},
		{"In", func() ast.Expr { return &ast.In{Expr: n(), List: n()} }},
		{"Case", func() ast.Expr {
			return &ast.Case{Operand: n(), Whens: []ast.CaseWhen{{Cond: n(), Result: n()}}, Else: n()}
		}},
		{"Index", func() ast.Expr { return &ast.Index{Base: n(), Idx: n()} }},
		{"Slice", func() ast.Expr { return &ast.Slice{Base: n(), From: n(), To: n()} }},
		{"MapLit", func() ast.Expr { return &ast.MapLit{Fields: []ast.MapField{{Key: "k", Val: n()}}} }},
		{"ListPred", func() ast.Expr { return &ast.ListPred{Var: "v", List: n(), Pred: n()} }},
		{"ListComp", func() ast.Expr { return &ast.ListComp{Var: "v", List: n(), Filter: n(), Map: n()} }},
		{"Reduce", func() ast.Expr { return &ast.Reduce{Acc: "a", Init: n(), Var: "v", List: n(), Body: n()} }},
		{"Exists", func() ast.Expr { return &ast.Exists{Pattern: pat(), Where: n()} }},
		{"CountSub", func() ast.Expr { return &ast.CountSub{Pattern: pat(), Where: n()} }},
		{"PatternComp", func() ast.Expr { return &ast.PatternComp{Pattern: pat(), Where: n(), Proj: n()} }},
		{"Cost", func() ast.Expr { return &ast.Cost{From: ref, To: "q"} }},
	}
}

// ExprKinds returns every ast.Expr kind declared anywhere in the ast
// package, by parsing the package source for isExpr method declarations
// with go/parser -- not a text pattern, and not a single-file assumption:
// a completeness tool with a quiet matching gap silently stops enforcing
// (the sibling engines both shipped that bug), so the scanner itself is
// pinned against the kind-case table by TestRollCallScannerAgreesWithTable.
func ExprKinds() ([]string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return nil, os.ErrNotExist
	}
	dir := filepath.Join(filepath.Dir(self), "..")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var kinds []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			fd, ok := d.(*goast.FuncDecl)
			if !ok || fd.Name.Name != "isExpr" || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			kinds = append(kinds, recvTypeName(fd.Recv.List[0].Type))
		}
	}
	slices.Sort(kinds)
	return kinds, nil
}

// recvTypeName unwraps a method receiver's type expression to its named
// type, through any pointer.
func recvTypeName(e goast.Expr) string {
	if star, ok := e.(*goast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*goast.Ident); ok {
		return id.Name
	}
	return ""
}

// MissingKinds returns the ast.Expr kinds covered does not contain,
// sorted -- the pure core of RollCall, exported so its discriminating
// power is testable directly (feed a deliberately incomplete map, assert
// the gap is reported).
func MissingKinds(covered map[string]bool) ([]string, error) {
	kinds, err := ExprKinds()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, k := range kinds {
		if !covered[k] {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

// RollCall fails unless covered contains every ast.Expr kind declared in
// the ast package, so adding a kind forces an explicit policy decision in
// every walker test that calls this. It also fails on covered kinds the
// scanner does NOT find: the two-sided comparison means a scanner
// matching gap surfaces as a table/scan disagreement instead of silently
// weakening enforcement.
func RollCall(t *testing.T, covered map[string]bool) {
	t.Helper()
	missing, err := MissingKinds(covered)
	if err != nil {
		t.Fatalf("scanning ast source: %v", err)
	}
	for _, k := range missing {
		t.Errorf("ast.%s has no walker case: add one and decide its policy", k)
	}
	kinds, _ := ExprKinds()
	for c := range covered {
		if !slices.Contains(kinds, c) {
			t.Errorf("walker case %q matches no ast.Expr kind the scanner found: renamed kind or scanner gap (%s)", c, strings.Join(kinds, ","))
		}
	}
}
