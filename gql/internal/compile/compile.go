// Expr -> CExpr lowering: hot leaves (slots, property reads) carry
// resolved indices/readers; correlated subqueries gain a result memo keyed
// on their outer reads; scalar functions with constant arguments fold at
// compile time; the remaining rarely-hot nodes fall back to the
// interpreter.
package compile

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Compiled is a compiled expression bound to a snapshot; it satisfies the
// executor's RowEval seam.
type Compiled struct {
	c cnode
	g *chickpeas.Snapshot
}

// Eval evaluates the compiled expression against a row -- identical in
// result to the interpreter on the source expression.
func (c *Compiled) Eval(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value {
	return ceval(ctx, c.c, c.g, row, slots)
}

// cnode is one compiled node.
type cnode interface{ isC() }

type cLit struct{ v value.Value }
type cSlot struct{ s int }
type cProp struct {
	slot   int
	reader propReader
}
type cNot struct{ e cnode }
type cNeg struct{ e cnode }
type cBin struct {
	op   ast.BinOp
	l, r cnode
}
type cList struct{ xs []cnode }
type cIn struct{ e, list cnode }

// cInConst is IN over a batch-constant list, lowered once to a prebuilt
// membership index.
type cInConst struct {
	e cnode
	m inMembership
}

// cInCarried is IN over a loop-invariant (carried, not batch-constant)
// list: the membership index rebuilds once per match-call epoch and is
// reused across that call's candidates. lastList short-circuits the
// rebuild entirely when the epoch's list is payload-identical to the
// previous one (a segment-stable slot).
type cInCarried struct {
	e, list  cnode
	epoch    uint64
	built    bool
	notList  bool
	m        inMembership
	lastList value.Value
}

type cIsNull struct {
	e       cnode
	negated bool
}

// cSubquery is a correlated EXISTS/COUNT with a result memo keyed on the
// outer slots it reads; memoSlots nil disables the memo (a nested
// subquery's dependencies can't be enumerated).
type cSubquery struct {
	pattern   *ast.Pattern
	where     ast.Expr
	isCount   bool
	memoSlots []int
	hasMemo   bool
	memo      map[string]int
	// memoI is the entity-id fast-path memo: when every correlated slot
	// holds a node id (<=2 of them), the key packs into a uint64 and a
	// miss's insert allocates nothing, unlike the byte-string memo. Both
	// are lazy; a subquery uses whichever its correlated slots select.
	memoI map[uint64]int
	// key is the reusable memo-key buffer: lookups probe string(key)
	// without allocating; only a miss's insert converts.
	key []byte
}

type cCase struct {
	operand cnode
	whens   [][2]cnode
	els     cnode
}
type cFunc struct {
	op   eval.FuncOp
	args []cnode
}

// cSlow defers to the interpreter (unrecognized functions, list
// machinery, map forms, label predicates, cost).
type cSlow struct{ e ast.Expr }

func (*cLit) isC()       {}
func (*cSlot) isC()      {}
func (*cProp) isC()      {}
func (*cNot) isC()       {}
func (*cNeg) isC()       {}
func (*cBin) isC()       {}
func (*cList) isC()      {}
func (*cIn) isC()        {}
func (*cInConst) isC()   {}
func (*cInCarried) isC() {}
func (*cIsNull) isC()    {}
func (*cSubquery) isC()  {}
func (*cCase) isC()      {}
func (*cFunc) isC()      {}
func (*cSlow) isC()      {}

// New compiles e against the segment's slots and snapshot. Parameters
// resolve at compile time (their values are constant for the execution).
func New(ctx *eval.Ctx, e ast.Expr, slots map[string]int, g *chickpeas.Snapshot) *Compiled {
	return &Compiled{c: comp(ctx, e, slots, g), g: g}
}

func comp(ctx *eval.Ctx, e ast.Expr, slots map[string]int, g *chickpeas.Snapshot) cnode {
	switch n := e.(type) {
	case *ast.Lit:
		return &cLit{v: eval.LitValue(ctx, n.Value)}
	case *ast.Var:
		if s, ok := slots[n.Name]; ok {
			return &cSlot{s: s}
		}
		return &cLit{v: value.Null()}
	case *ast.Prop:
		if s, ok := slots[n.Var]; ok {
			return &cProp{slot: s, reader: newPropReader(g, n.Key)}
		}
		return &cLit{v: value.Null()}
	case *ast.Unary:
		c := comp(ctx, n.Expr, slots, g)
		if n.Op == ast.Not {
			return &cNot{e: c}
		}
		return &cNeg{e: c}
	case *ast.Binary:
		return &cBin{op: n.Op, l: comp(ctx, n.LHS, slots, g), r: comp(ctx, n.RHS, slots, g)}
	case *ast.ListExpr:
		xs := make([]cnode, len(n.Elems))
		for i, el := range n.Elems {
			xs[i] = comp(ctx, el, slots, g)
		}
		return &cList{xs: xs}
	case *ast.In:
		return &cIn{e: comp(ctx, n.Expr, slots, g), list: comp(ctx, n.List, slots, g)}
	case *ast.IsNull:
		return &cIsNull{e: comp(ctx, n.Expr, slots, g), negated: n.Negated}
	case *ast.Exists:
		ms, ok := correlatedSlots(n.Pattern, n.Where, slots)
		return &cSubquery{pattern: n.Pattern, where: n.Where, memoSlots: ms, hasMemo: ok, memo: map[string]int{}}
	case *ast.CountSub:
		ms, ok := correlatedSlots(n.Pattern, n.Where, slots)
		return &cSubquery{pattern: n.Pattern, where: n.Where, isCount: true, memoSlots: ms, hasMemo: ok, memo: map[string]int{}}
	case *ast.Case:
		c := &cCase{whens: make([][2]cnode, len(n.Whens))}
		if n.Operand != nil {
			c.operand = comp(ctx, n.Operand, slots, g)
		}
		for i, w := range n.Whens {
			c.whens[i] = [2]cnode{comp(ctx, w.Cond, slots, g), comp(ctx, w.Result, slots, g)}
		}
		if n.Else != nil {
			c.els = comp(ctx, n.Else, slots, g)
		}
		return c
	case *ast.Func:
		// A recognized scalar function with positional args compiles (and
		// constant-folds); DISTINCT/star forms and unknown names stay
		// interpreted.
		if op, ok := eval.ResolveFuncOp(n.Name); ok && !n.Distinct && !n.Star {
			args := make([]cnode, len(n.Args))
			for i, a := range n.Args {
				args[i] = comp(ctx, a, slots, g)
			}
			return foldFunc(op, args)
		}
		return &cSlow{e: e}
	default:
		// Cost (kernel dispatch lands with M17), list predicates,
		// comprehensions, reduce, index/slice, property-of-expression,
		// map forms, label predicates: interpreter-backed.
		return &cSlow{e: e}
	}
}

// foldFunc constant-folds a scalar function whose arguments are all
// compile-time constants -- every FuncOp is deterministic, so this is
// result-identical to per-row evaluation (the dominant win is a temporal
// constructor in a hot filter parsing its ISO string once).
func foldFunc(op eval.FuncOp, args []cnode) cnode {
	argv := make([]value.Value, len(args))
	for i, a := range args {
		lit, ok := a.(*cLit)
		if !ok {
			return &cFunc{op: op, args: args}
		}
		argv[i] = lit.v
	}
	return &cLit{v: eval.ApplyFunc(op, argv)}
}

// correlatedSlots is the outer slots a correlated subquery reads -- its
// memo key. ok=false when the dependency set can't be fully determined (a
// nested subquery), which disables memoization (still correct, evaluated
// per row).
func correlatedSlots(p *ast.Pattern, where ast.Expr, slots map[string]int) ([]int, bool) {
	seen := map[int]struct{}{}
	add := func(v string) {
		if v == "" {
			return
		}
		if s, ok := slots[v]; ok {
			seen[s] = struct{}{}
		}
	}
	add(p.Start.Var)
	for i := range p.Hops {
		add(p.Hops[i].Rel.Var)
		add(p.Hops[i].Node.Var)
	}
	ok := true
	if where != nil {
		ok = collectOuterSlots(where, slots, add)
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	// Deterministic key order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if !ok {
		return nil, false
	}
	return out, true
}

// collectOuterSlots adds the outer slots e references; returns false when
// e contains a construct whose outer dependencies can't be enumerated (a
// nested EXISTS/COUNT, or a pattern comprehension's correlation).
func collectOuterSlots(e ast.Expr, slots map[string]int, add func(string)) bool {
	switch n := e.(type) {
	case *ast.Lit:
		return true
	case *ast.Var:
		add(n.Name)
		return true
	case *ast.Prop:
		add(n.Var)
		return true
	case *ast.Cost:
		add(n.From)
		add(n.To)
		return true
	case *ast.Unary:
		return collectOuterSlots(n.Expr, slots, add)
	case *ast.IsNull:
		return collectOuterSlots(n.Expr, slots, add)
	case *ast.Binary:
		return collectOuterSlots(n.LHS, slots, add) && collectOuterSlots(n.RHS, slots, add)
	case *ast.In:
		return collectOuterSlots(n.Expr, slots, add) && collectOuterSlots(n.List, slots, add)
	case *ast.ListPred:
		return collectOuterSlots(n.List, slots, add) && collectOuterSlots(n.Pred, slots, add)
	case *ast.Reduce:
		return collectOuterSlots(n.Init, slots, add) &&
			collectOuterSlots(n.List, slots, add) &&
			collectOuterSlots(n.Body, slots, add)
	case *ast.ListComp:
		ok := collectOuterSlots(n.List, slots, add)
		if n.Filter != nil {
			ok = collectOuterSlots(n.Filter, slots, add) && ok
		}
		if n.Map != nil {
			ok = collectOuterSlots(n.Map, slots, add) && ok
		}
		return ok
	case *ast.ListExpr:
		for _, el := range n.Elems {
			if !collectOuterSlots(el, slots, add) {
				return false
			}
		}
		return true
	case *ast.PatternComp:
		// The comprehension's correlation structure isn't modeled here;
		// collect what the filter/proj reference, then report incomplete.
		if n.Where != nil {
			collectOuterSlots(n.Where, slots, add)
		}
		collectOuterSlots(n.Proj, slots, add)
		return false
	case *ast.Func:
		for _, a := range n.Args {
			if !collectOuterSlots(a, slots, add) {
				return false
			}
		}
		return true
	case *ast.Case:
		ok := true
		if n.Operand != nil {
			ok = collectOuterSlots(n.Operand, slots, add)
		}
		for _, w := range n.Whens {
			ok = collectOuterSlots(w.Cond, slots, add) && ok
			ok = collectOuterSlots(w.Result, slots, add) && ok
		}
		if n.Else != nil {
			ok = collectOuterSlots(n.Else, slots, add) && ok
		}
		return ok
	case *ast.Index:
		return collectOuterSlots(n.Base, slots, add) && collectOuterSlots(n.Idx, slots, add)
	case *ast.Slice:
		ok := collectOuterSlots(n.Base, slots, add)
		if n.From != nil {
			ok = collectOuterSlots(n.From, slots, add) && ok
		}
		if n.To != nil {
			ok = collectOuterSlots(n.To, slots, add) && ok
		}
		return ok
	case *ast.PropOf:
		return collectOuterSlots(n.Base, slots, add)
	case *ast.MapProj:
		add(n.Var)
		for _, en := range n.Entries {
			if en.Kind == ast.MapProjField && !collectOuterSlots(en.Expr, slots, add) {
				return false
			}
		}
		return true
	case *ast.MapLit:
		for _, f := range n.Fields {
			if !collectOuterSlots(f.Val, slots, add) {
				return false
			}
		}
		return true
	case *ast.HasLabelExpr:
		add(n.Var)
		return true
	default:
		// A nested subquery's correlated reads aren't enumerated -> no memo.
		return false
	}
}
