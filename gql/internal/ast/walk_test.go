// AST helper tests: the walker visits every node kind (the desugar /
// binder / autoparam passes depend on it), pattern reversal flips hops and
// directions, and every node satisfies its marker interface.
package ast

import "testing"

func u(n uint64) *uint64 { return &n }

// megaExpr builds one expression containing every Expr node kind.
func megaExpr() Expr {
	pat := &Pattern{
		Start: NodePat{Var: "a", PropExprs: []PropExprEntry{{Key: "k", Val: &Var{Name: "v0"}}}},
		Hops: []PatternHop{{
			Rel:  RelPat{Dir: DirOut, Types: []string{"T"}, PropExprs: []PropExprEntry{{Key: "rk", Val: &Var{Name: "v1"}}}},
			Node: NodePat{Var: "b", PropExprs: []PropExprEntry{{Key: "nk", Val: &Var{Name: "v2"}}}},
		}},
	}
	return &Binary{
		Op: OpAnd,
		LHS: &Case{
			Operand: &Prop{Var: "a", Key: "x"},
			Whens:   []CaseWhen{{Cond: &Lit{Value: IntLit(1)}, Result: &Unary{Op: Neg, Expr: &Var{Name: "w"}}}},
			Else:    &IsNull{Expr: &Index{Base: &ListExpr{Elems: []Expr{&Lit{Value: StrLit("s")}}}, Idx: &Lit{Value: IntLit(0)}}},
		},
		RHS: &In{
			Expr: &Slice{Base: &PropOf{Base: &Func{Name: "rels", Args: []Expr{&Var{Name: "e"}}}, Key: "ts"}, From: &Lit{Value: IntLit(0)}},
			List: &ListComp{
				Var:    "x",
				List:   &Reduce{Acc: "acc", Init: &Lit{Value: IntLit(0)}, Var: "y", List: &Var{Name: "ys"}, Body: &Var{Name: "acc"}},
				Filter: &Exists{Pattern: pat, Where: &CountSub{Pattern: pat, Where: nil}},
				Map: &Binary{Op: OpAdd,
					LHS: &ListPred{Quant: QuantAll, Var: "z", List: &Var{Name: "zs"}, Pred: &PatternComp{Pattern: pat, Where: &Var{Name: "c"}, Proj: &Var{Name: "d"}}},
					RHS: &MapLit{Fields: []MapField{{Key: "m", Val: &MapProj{Var: "n", Entries: []MapProjEntry{
						{Kind: MapProjProp, Key: "p"},
						{Kind: MapProjField, Key: "f", Expr: &HasLabelExpr{Var: "n", Expr: &LabelExpr{Kind: LabelName, Name: "L"}}},
						{Kind: MapProjAll},
					}}}}},
				},
			},
		},
	}
}

func TestWalkVisitsEveryNodeKind(t *testing.T) {
	seen := map[string]int{}
	name := func(e Expr) string {
		switch e.(type) {
		case *Lit:
			return "Lit"
		case *Var:
			return "Var"
		case *Prop:
			return "Prop"
		case *Unary:
			return "Unary"
		case *Binary:
			return "Binary"
		case *Func:
			return "Func"
		case *ListExpr:
			return "ListExpr"
		case *In:
			return "In"
		case *IsNull:
			return "IsNull"
		case *Case:
			return "Case"
		case *Exists:
			return "Exists"
		case *CountSub:
			return "CountSub"
		case *ListPred:
			return "ListPred"
		case *Reduce:
			return "Reduce"
		case *ListComp:
			return "ListComp"
		case *PatternComp:
			return "PatternComp"
		case *Index:
			return "Index"
		case *Slice:
			return "Slice"
		case *PropOf:
			return "PropOf"
		case *MapProj:
			return "MapProj"
		case *MapLit:
			return "MapLit"
		case *HasLabelExpr:
			return "HasLabelExpr"
		}
		return "?"
	}
	Walk(megaExpr(), func(e Expr) bool {
		seen[name(e)]++
		return true
	})
	for _, want := range []string{
		"Lit", "Var", "Prop", "Unary", "Binary", "Func", "ListExpr", "In",
		"IsNull", "Case", "Exists", "CountSub", "ListPred", "Reduce",
		"ListComp", "PatternComp", "Index", "Slice", "PropOf", "MapProj",
		"MapLit", "HasLabelExpr",
	} {
		if seen[want] == 0 {
			t.Fatalf("walker never visited %s (saw %v)", want, seen)
		}
	}
	// A Cost node's expression weight is descended into.
	visited := false
	Walk(&Cost{Weight: CostSpec{Kind: CostExpr, Expr: &Var{Name: "w"}}}, func(e Expr) bool {
		if v, ok := e.(*Var); ok && v.Name == "w" {
			visited = true
		}
		return true
	})
	if !visited {
		t.Fatal("cost weight expression not visited")
	}
	// Returning false prunes children.
	count := 0
	Walk(megaExpr(), func(e Expr) bool { count++; return false })
	if count != 1 {
		t.Fatalf("pruned walk visited %d nodes", count)
	}
	// Nil expressions are ignored.
	Walk(nil, func(Expr) bool { t.Fatal("nil visited"); return true })
}

func TestPatternReversedAndEndNode(t *testing.T) {
	p := Pattern{
		Start: NodePat{Var: "a"},
		Hops: []PatternHop{
			{Rel: RelPat{Dir: DirOut, Types: []string{"R1"}}, Node: NodePat{Var: "b"}},
			{Rel: RelPat{Dir: DirIn, Types: []string{"R2"}, Length: &VarLength{Min: u(1), Max: u(3)}}, Node: NodePat{Var: "c"}},
		},
	}
	if p.EndNode().Var != "c" {
		t.Fatalf("end = %q", p.EndNode().Var)
	}
	r := p.Reversed()
	if r.Start.Var != "c" || r.EndNode().Var != "a" {
		t.Fatalf("reversed ends = %q..%q", r.Start.Var, r.EndNode().Var)
	}
	if r.Hops[0].Rel.Dir != DirOut || r.Hops[0].Rel.Types[0] != "R2" || r.Hops[0].Node.Var != "b" {
		t.Fatalf("reversed hop0 = %+v", r.Hops[0])
	}
	if r.Hops[1].Rel.Dir != DirIn || r.Hops[1].Rel.Types[0] != "R1" || r.Hops[1].Node.Var != "a" {
		t.Fatalf("reversed hop1 = %+v", r.Hops[1])
	}
	// The quantifier rides with its relationship.
	if r.Hops[0].Rel.Length == nil || *r.Hops[0].Rel.Length.Max != 3 {
		t.Fatalf("reversed length = %+v", r.Hops[0].Rel.Length)
	}
	// A hopless pattern reverses to itself.
	single := Pattern{Start: NodePat{Var: "x"}}
	if single.Reversed().Start.Var != "x" || single.EndNode().Var != "x" {
		t.Fatal("single-node reversal")
	}
	// Both stays Both.
	if DirBoth.Flipped() != DirBoth || DirOut.Flipped() != DirIn || DirIn.Flipped() != DirOut {
		t.Fatal("Flipped")
	}
}

func TestMarkerInterfaces(t *testing.T) {
	exprs := []Expr{
		&Lit{}, &Var{}, &Prop{}, &Unary{}, &Binary{}, &Func{}, &ListExpr{},
		&In{}, &IsNull{}, &Case{}, &Cost{}, &Exists{}, &CountSub{},
		&ListPred{}, &Reduce{}, &ListComp{}, &PatternComp{}, &Index{},
		&Slice{}, &PropOf{}, &MapProj{}, &MapLit{}, &HasLabelExpr{},
	}
	for _, e := range exprs {
		e.isExpr()
	}
	clauses := []Clause{
		&Match{}, &With{}, &ShortestPath{}, &CallProc{}, &PathBind{},
		&Unwind{}, &CallSubquery{},
	}
	for _, c := range clauses {
		c.isClause()
	}
	if ParamLit(3).P != 3 || ParamLit(3).Kind != LitParam {
		t.Fatal("ParamLit")
	}
}
