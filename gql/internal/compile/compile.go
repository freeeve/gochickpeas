// Expr -> CExpr lowering: hot leaves (slots, property reads) carry
// resolved indices/readers; correlated subqueries gain a result memo keyed
// on their outer reads; scalar functions with constant arguments fold at
// compile time; the remaining rarely-hot nodes fall back to the
// interpreter.
package compile

import (
	"maps"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Compiled is a compiled expression bound to a snapshot; it satisfies the
// executor's RowEval seam. fast, when derivable, is the whole-expression
// monomorphic form (rowfast.go), result-identical to the tree.
type Compiled struct {
	c    cnode
	g    *chickpeas.Snapshot
	fast rowFast
}

// newCompiled binds a lowered tree, deriving the monomorphic fast form
// when the root shape supports it. Every construction site (initial
// lowering and the hoisting rewrites) routes through here so the fast
// form always reflects the final tree.
func newCompiled(c cnode, g *chickpeas.Snapshot) *Compiled {
	return &Compiled{c: c, g: g, fast: deriveRowFast(c, g)}
}

// Eval evaluates the compiled expression against a row -- identical in
// result to the interpreter on the source expression.
func (c *Compiled) Eval(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value {
	if c.fast != nil {
		return c.fast(ctx, row, slots)
	}
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

// cCmpPropConst fuses <prop> <comparison> <constant> -- the dominant
// filter shape -- into one node: the hoisted column read and the shared
// value.Compare run with no child dispatch. rev marks a constant left
// operand. Semantics are identical to the unfused cBin: same reader,
// same Compare coercions, same three-valued collapse.
type cCmpPropConst struct {
	prop *cProp
	c    value.Value
	op   ast.BinOp
	rev  bool
}

// cSlow defers to the interpreter (unrecognized functions, list
// machinery, map forms, label predicates, cost).
type cSlow struct{ e ast.Expr }

func (*cLit) isC()          {}
func (*cSlot) isC()         {}
func (*cProp) isC()         {}
func (*cCmpPropConst) isC() {}
func (*cNot) isC()          {}
func (*cNeg) isC()          {}
func (*cBin) isC()          {}
func (*cList) isC()         {}
func (*cIn) isC()           {}
func (*cInConst) isC()      {}
func (*cInCarried) isC()    {}
func (*cIsNull) isC()       {}
func (*cSubquery) isC()     {}
func (*cCase) isC()         {}
func (*cFunc) isC()         {}
func (*cSlow) isC()         {}

// New compiles e against the segment's slots and snapshot. Parameters
// resolve at compile time (their values are constant for the execution).
func New(ctx *eval.Ctx, e ast.Expr, slots map[string]int, g *chickpeas.Snapshot) *Compiled {
	return newCompiled(comp(ctx, e, slots, g), g)
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
		var built cnode
		if n.Op == ast.Not {
			built = &cNot{e: c}
		} else {
			built = &cNeg{e: c}
		}
		return foldLits(ctx, built, g, c)
	case *ast.Binary:
		l, r := comp(ctx, n.LHS, slots, g), comp(ctx, n.RHS, slots, g)
		built := foldLits(ctx, &cBin{op: n.Op, l: l, r: r}, g, l, r)
		if fused := fuseCmpPropConst(n.Op, built, l, r); fused != nil {
			return fused
		}
		return built
	case *ast.ListExpr:
		xs := make([]cnode, len(n.Elems))
		for i, el := range n.Elems {
			xs[i] = comp(ctx, el, slots, g)
		}
		return foldLits(ctx, &cList{xs: xs}, g, xs...)
	case *ast.In:
		e2, list := comp(ctx, n.Expr, slots, g), comp(ctx, n.List, slots, g)
		return foldLits(ctx, &cIn{e: e2, list: list}, g, e2, list)
	case *ast.IsNull:
		c := comp(ctx, n.Expr, slots, g)
		return foldLits(ctx, &cIsNull{e: c, negated: n.Negated}, g, c)
	case *ast.Exists:
		ms, ok := correlatedSlots(n.Pattern, n.Where, slots)
		// A single fixed hop with both endpoints outer-bound and no WHERE
		// answers in near-constant time (a bound-pair relationship count
		// off the edge-key sets), so memoizing it costs more than it
		// saves: the memo map for a large outer row set dwarfs every
		// other execution allocation while each hit barely beats the
		// probe it caches.
		if ok && cheapExistsProbe(n.Pattern, n.Where, slots) {
			ok = false
		}
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
		// map forms, label predicates: interpreter-backed -- but a
		// row-independent subtree still folds to its literal (the dominant
		// win is a constant map argument to a temporal constructor in a
		// hot filter, e.g. duration({hours: 4}), which otherwise
		// rebuilds map + value per row).
		if constExpr(e, nil) {
			return &cLit{v: eval.Eval(ctx, e, nil, nil)}
		}
		return &cSlow{e: e}
	}
}

// fuseCmpPropConst collapses <prop> <comparison> <constant> (either
// operand order) into a cCmpPropConst when the binary did not already
// fold to a literal; nil when the shape doesn't apply.
func fuseCmpPropConst(op ast.BinOp, built, l, r cnode) cnode {
	switch op {
	case ast.OpEq, ast.OpNeq, ast.OpLt, ast.OpLte, ast.OpGt, ast.OpGte:
	default:
		return nil
	}
	if _, folded := built.(*cLit); folded {
		return nil
	}
	if p, ok := l.(*cProp); ok {
		if c, isLit := r.(*cLit); isLit {
			return &cCmpPropConst{prop: p, c: c.v, op: op}
		}
	}
	if p, ok := r.(*cProp); ok {
		if c, isLit := l.(*cLit); isLit {
			return &cCmpPropConst{prop: p, c: c.v, op: op, rev: true}
		}
	}
	return nil
}

// foldLits replaces built with its evaluated literal when every input is
// already a literal -- the cnode ops are pure, so one ceval with no row is
// result-identical to per-row evaluation. Folding composes bottom-up
// through comp, so nested constant expressions collapse to one cLit.
func foldLits(ctx *eval.Ctx, built cnode, g *chickpeas.Snapshot, inputs ...cnode) cnode {
	for _, in := range inputs {
		if _, ok := in.(*cLit); !ok {
			return built
		}
	}
	return &cLit{v: ceval(ctx, built, g, nil, nil)}
}

// constExpr reports whether e reads nothing from the row or graph -- no
// free variable (iteration variables bound by an enclosing list scope
// pass), no property access, no subquery, pattern, path, or label form --
// so it evaluates identically for every row. Parameters count as constant:
// New resolves them at compile time, once per execution. Scalar functions
// are all deterministic; startNode/endNode sit outside ResolveFuncOp and
// so fail the resolution check.
func constExpr(e ast.Expr, bound map[string]bool) bool {
	switch n := e.(type) {
	case *ast.Lit:
		return true
	case *ast.Var:
		return bound[n.Name]
	case *ast.Unary:
		return constExpr(n.Expr, bound)
	case *ast.Binary:
		return constExpr(n.LHS, bound) && constExpr(n.RHS, bound)
	case *ast.ListExpr:
		for _, el := range n.Elems {
			if !constExpr(el, bound) {
				return false
			}
		}
		return true
	case *ast.MapLit:
		for _, f := range n.Fields {
			if !constExpr(f.Val, bound) {
				return false
			}
		}
		return true
	case *ast.In:
		return constExpr(n.Expr, bound) && constExpr(n.List, bound)
	case *ast.IsNull:
		return constExpr(n.Expr, bound)
	case *ast.Index:
		return constExpr(n.Base, bound) && constExpr(n.Idx, bound)
	case *ast.Slice:
		return constExpr(n.Base, bound) &&
			(n.From == nil || constExpr(n.From, bound)) &&
			(n.To == nil || constExpr(n.To, bound))
	case *ast.PropOf:
		return constExpr(n.Base, bound)
	case *ast.Case:
		if n.Operand != nil && !constExpr(n.Operand, bound) {
			return false
		}
		for _, w := range n.Whens {
			if !constExpr(w.Cond, bound) || !constExpr(w.Result, bound) {
				return false
			}
		}
		return n.Else == nil || constExpr(n.Else, bound)
	case *ast.Func:
		if n.Distinct || n.Star {
			return false
		}
		if _, ok := eval.ResolveFuncOp(n.Name); !ok {
			return false
		}
		for _, a := range n.Args {
			if !constExpr(a, bound) {
				return false
			}
		}
		return true
	case *ast.ListPred:
		return constExpr(n.List, bound) && constExpr(n.Pred, boundWith(bound, n.Var))
	case *ast.Reduce:
		return constExpr(n.List, bound) && constExpr(n.Init, bound) &&
			constExpr(n.Body, boundWith(bound, n.Acc, n.Var))
	case *ast.ListComp:
		if !constExpr(n.List, bound) {
			return false
		}
		inner := boundWith(bound, n.Var)
		return (n.Filter == nil || constExpr(n.Filter, inner)) &&
			(n.Map == nil || constExpr(n.Map, inner))
	default:
		return false
	}
}

// boundWith extends a bound-variable set with list-scope iteration names.
func boundWith(bound map[string]bool, vars ...string) map[string]bool {
	out := make(map[string]bool, len(bound)+len(vars))
	maps.Copy(out, bound)
	for _, v := range vars {
		out[v] = true
	}
	return out
}

// foldFunc constant-folds a scalar function whose arguments are all
// compile-time constants -- every FuncOp is deterministic, so this is
// result-identical to per-row evaluation (the dominant win is a temporal
// constructor in a hot filter parsing its ISO string once).
// foldFunc evaluates a scalar function at plan time when every argument is a
// literal, so a hot filter's constant call (e.g. duration({hours: 4})) is one
// value instead of a per-row rebuild. A ZERO-arg call folds vacuously (the
// loop below is empty) -- sound only because every scalar function here is
// deterministic: the only zero-arg forms, date()/datetime()/localdatetime(),
// return Null, not the current instant. A volatile function (rand/timestamp)
// must NOT reach this fold -- its plan-time value would be baked into the
// cached plan and replayed forever -- so it would need excluding here and at
// constExpr both (the two fold sites). Locked by the gql package's zero-arg
// fold tests.
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

// cheapExistsProbe reports the subquery shape whose evaluation is a
// near-constant-time bound-pair probe: one fixed-length hop, both
// endpoint variables bound in the outer scope, no inner WHERE.
func cheapExistsProbe(p *ast.Pattern, where ast.Expr, slots map[string]int) bool {
	if where != nil || len(p.Hops) != 1 || p.Hops[0].Rel.Length != nil {
		return false
	}
	startBound := p.Start.Var != "" && hasSlot(slots, p.Start.Var)
	endBound := p.Hops[0].Node.Var != "" && hasSlot(slots, p.Hops[0].Node.Var)
	return startBound && endBound
}

func hasSlot(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
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
