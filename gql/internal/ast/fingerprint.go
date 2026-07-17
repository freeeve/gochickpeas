// Fingerprint renders a query AST to a canonical string: two ASTs render
// identically iff they are structurally identical (every node emits a type
// tag with delimited fields, strings are quoted), so the rendering is safe
// as a plan-cache template key -- the Go analogue of the Rust engine
// keying its L2 on the template AST's debug rendering.
package ast

import (
	"strconv"
	"strings"
)

// Fingerprint is the canonical rendering of q.
func Fingerprint(q *Query) string {
	var b strings.Builder
	fpQuery(&b, q)
	return b.String()
}

func fpQuery(b *strings.Builder, q *Query) {
	b.WriteString("Q")
	b.WriteString(strconv.Itoa(int(q.Mode)))
	for i, part := range q.Parts {
		if i > 0 {
			b.WriteString("|U")
			b.WriteString(strconv.Itoa(int(q.Union[i-1])))
			b.WriteByte('|')
		}
		for j := range part.Clauses {
			fpClause(b, part.Clauses[j])
		}
		fpProjection(b, &part.Ret)
	}
}

func fpClause(b *strings.Builder, c Clause) {
	switch n := c.(type) {
	case *Match:
		b.WriteString(";M")
		if n.Optional {
			b.WriteByte('?')
		}
		if n.Acyclic {
			b.WriteByte('a')
		}
		if n.Repeatable {
			// REPEATABLE ELEMENTS changes the clause's relationship-
			// uniqueness semantics; without this byte a walk-mode query
			// shared a template (and its plan) with its TRAIL twin.
			b.WriteByte('r')
		}
		for i := range n.Patterns {
			fpPattern(b, &n.Patterns[i])
		}
		fpWhere(b, n.Where)
	case *With:
		b.WriteString(";W")
		fpProjection(b, &n.Proj)
		fpWhere(b, n.Where)
	case *ShortestPath:
		b.WriteString(";SP")
		if n.Optional {
			b.WriteByte('?')
		}
		if n.All {
			b.WriteByte('*')
		}
		b.WriteString(strconv.Quote(n.PathVar))
		fpPattern(b, &n.Pattern)
		if n.Weight != nil {
			b.WriteString("w(")
			b.WriteString(strconv.Itoa(int(n.Weight.Kind)))
			b.WriteString(strconv.Quote(n.Weight.Prop))
			b.WriteString(strconv.FormatFloat(n.Weight.Const, 'g', -1, 64))
			fpExpr(b, n.Weight.Expr)
			b.WriteByte(')')
		}
		fpWhere(b, n.Where)
	case *CallProc:
		b.WriteString(";C")
		b.WriteString(strconv.Quote(n.Proc))
		for i := range n.Args {
			fpExpr(b, n.Args[i])
		}
		for _, y := range n.Yields {
			b.WriteString("y")
			b.WriteString(strconv.Quote(y.Field))
			b.WriteString(strconv.Quote(y.Alias))
		}
	case *PathBind:
		b.WriteString(";PB")
		if n.Optional {
			b.WriteByte('?')
		}
		if n.Acyclic {
			b.WriteByte('a')
		}
		b.WriteString(strconv.Quote(n.PathVar))
		fpPattern(b, &n.Pattern)
		fpWhere(b, n.Where)
	case *Unwind:
		b.WriteString(";F")
		b.WriteString(strconv.Quote(n.Var))
		fpExpr(b, n.Expr)
	case *CallSubquery:
		b.WriteString(";CS[")
		for _, im := range n.Imports {
			b.WriteString(strconv.Quote(im))
		}
		b.WriteByte(']')
		fpQuery(b, &n.Query)
	}
}

func fpProjection(b *strings.Builder, p *Projection) {
	b.WriteString("{P")
	if p.Star {
		b.WriteByte('*')
	}
	if p.Distinct {
		b.WriteByte('D')
	}
	for i := range p.Items {
		fpExpr(b, p.Items[i].Expr)
		b.WriteString(strconv.Quote(p.Items[i].Alias))
	}
	for i := range p.OrderBy {
		b.WriteString("o")
		fpExpr(b, p.OrderBy[i].Expr)
		if p.OrderBy[i].Desc {
			b.WriteByte('v')
		}
	}
	// OFFSET/LIMIT counts are never auto-lifted, so they belong to the
	// template identity.
	if p.Skip != nil {
		b.WriteString("s")
		b.WriteString(strconv.FormatUint(*p.Skip, 10))
	}
	if p.Limit != nil {
		b.WriteString("l")
		b.WriteString(strconv.FormatUint(*p.Limit, 10))
	}
	b.WriteByte('}')
}

func fpWhere(b *strings.Builder, w Expr) {
	if w != nil {
		b.WriteString("w")
		fpExpr(b, w)
	}
}

func fpPattern(b *strings.Builder, p *Pattern) {
	b.WriteString("(")
	fpNodePat(b, &p.Start)
	for i := range p.Hops {
		fpRelPat(b, &p.Hops[i].Rel)
		fpNodePat(b, &p.Hops[i].Node)
	}
	b.WriteByte(')')
}

func fpNodePat(b *strings.Builder, n *NodePat) {
	b.WriteString("n")
	b.WriteString(strconv.Quote(n.Var))
	for _, l := range n.Labels {
		b.WriteString(strconv.Quote(l))
	}
	if n.LabelExpr != nil {
		fpLabelExpr(b, n.LabelExpr)
	}
	for i := range n.Props {
		b.WriteString(strconv.Quote(n.Props[i].Key))
		fpLiteral(b, &n.Props[i].Val)
	}
	for i := range n.PropExprs {
		b.WriteString(strconv.Quote(n.PropExprs[i].Key))
		fpExpr(b, n.PropExprs[i].Val)
	}
}

func fpRelPat(b *strings.Builder, r *RelPat) {
	b.WriteString("r")
	b.WriteString(strconv.Itoa(int(r.Dir)))
	b.WriteString(strconv.Quote(r.Var))
	for _, t := range r.Types {
		b.WriteString(strconv.Quote(t))
	}
	if r.Length != nil {
		b.WriteByte('q')
		if r.Length.Min != nil {
			b.WriteString(strconv.FormatUint(*r.Length.Min, 10))
		}
		b.WriteByte(',')
		if r.Length.Max != nil {
			b.WriteString(strconv.FormatUint(*r.Length.Max, 10))
		}
	}
	for i := range r.Props {
		b.WriteString(strconv.Quote(r.Props[i].Key))
		fpLiteral(b, &r.Props[i].Val)
	}
	for i := range r.PropExprs {
		b.WriteString(strconv.Quote(r.PropExprs[i].Key))
		fpExpr(b, r.PropExprs[i].Val)
	}
}

func fpLabelExpr(b *strings.Builder, l *LabelExpr) {
	b.WriteString("L")
	b.WriteString(strconv.Itoa(int(l.Kind)))
	b.WriteString(strconv.Quote(l.Name))
	if l.L != nil {
		fpLabelExpr(b, l.L)
	}
	if l.R != nil {
		fpLabelExpr(b, l.R)
	}
	b.WriteByte('.')
}

func fpLiteral(b *strings.Builder, l *Literal) {
	b.WriteString("#")
	b.WriteString(strconv.Itoa(int(l.Kind)))
	switch l.Kind {
	case LitInt:
		b.WriteString(strconv.FormatInt(l.I, 10))
	case LitFloat:
		b.WriteString(strconv.FormatFloat(l.F, 'g', -1, 64))
	case LitStr, LitNamedParam:
		b.WriteString(strconv.Quote(l.S))
	case LitBool:
		if l.B {
			b.WriteByte('t')
		} else {
			b.WriteByte('f')
		}
	case LitParam:
		b.WriteString(strconv.FormatUint(uint64(l.P), 10))
	}
}

func fpExpr(b *strings.Builder, e Expr) {
	switch n := e.(type) {
	case nil:
		b.WriteString("_")
	case *Lit:
		fpLiteral(b, &n.Value)
	case *Var:
		b.WriteString("v")
		b.WriteString(strconv.Quote(n.Name))
	case *Prop:
		b.WriteString("p")
		b.WriteString(strconv.Quote(n.Var))
		b.WriteString(strconv.Quote(n.Key))
	case *Unary:
		b.WriteString("u")
		b.WriteString(strconv.Itoa(int(n.Op)))
		fpExpr(b, n.Expr)
	case *Binary:
		b.WriteString("b")
		b.WriteString(strconv.Itoa(int(n.Op)))
		fpExpr(b, n.LHS)
		fpExpr(b, n.RHS)
	case *Func:
		b.WriteString("f")
		b.WriteString(strconv.Quote(n.Name))
		if n.Distinct {
			b.WriteByte('D')
		}
		if n.Star {
			b.WriteByte('*')
		}
		for _, a := range n.Args {
			fpExpr(b, a)
		}
		b.WriteByte('.')
	case *ListExpr:
		b.WriteString("[")
		for _, el := range n.Elems {
			fpExpr(b, el)
		}
		b.WriteByte(']')
	case *In:
		b.WriteString("i")
		fpExpr(b, n.Expr)
		fpExpr(b, n.List)
	case *IsNull:
		b.WriteString("z")
		if n.Negated {
			b.WriteByte('!')
		}
		fpExpr(b, n.Expr)
	case *IsTruth:
		// Missing cases here are CORRECTNESS bugs, not cosmetics: an expr
		// node the fingerprint cannot see hashes to the same "?" as any
		// other, so two different queries share one cached template plan
		// (found: 1 IS TYPED FLOAT returned 1.5's answer).
		b.WriteString("zt")
		if n.Want {
			b.WriteByte('T')
		} else {
			b.WriteByte('F')
		}
		if n.Negated {
			b.WriteByte('!')
		}
		fpExpr(b, n.Expr)
	case *IsTyped:
		b.WriteString("zk")
		b.WriteString(strconv.Quote(n.Kind))
		if n.Negated {
			b.WriteByte('!')
		}
		fpExpr(b, n.Expr)
	case *Case:
		b.WriteString("c")
		fpExpr(b, n.Operand)
		for _, w := range n.Whens {
			fpExpr(b, w.Cond)
			fpExpr(b, w.Result)
		}
		fpExpr(b, n.Else)
		b.WriteByte('.')
	case *Cost:
		b.WriteString("$")
		b.WriteString(strconv.Quote(n.From))
		b.WriteString(strconv.Quote(n.To))
		b.WriteString(strconv.Itoa(int(n.Dir)))
		for _, t := range n.Types {
			b.WriteString(strconv.Quote(t))
		}
		b.WriteString(strconv.Itoa(int(n.Weight.Kind)))
		b.WriteString(strconv.Quote(n.Weight.Prop))
		b.WriteString(strconv.FormatFloat(n.Weight.Const, 'g', -1, 64))
		fpExpr(b, n.Weight.Expr)
	case *Exists:
		b.WriteString("e")
		fpPattern(b, n.Pattern)
		fpWhere(b, n.Where)
		b.WriteByte('.')
	case *CountSub:
		b.WriteString("k")
		fpPattern(b, n.Pattern)
		fpWhere(b, n.Where)
		b.WriteByte('.')
	case *ListPred:
		b.WriteString("q")
		b.WriteString(strconv.Itoa(int(n.Quant)))
		b.WriteString(strconv.Quote(n.Var))
		fpExpr(b, n.List)
		fpExpr(b, n.Pred)
	case *Reduce:
		b.WriteString("R")
		b.WriteString(strconv.Quote(n.Acc))
		fpExpr(b, n.Init)
		b.WriteString(strconv.Quote(n.Var))
		fpExpr(b, n.List)
		fpExpr(b, n.Body)
	case *ListComp:
		b.WriteString("lc")
		b.WriteString(strconv.Quote(n.Var))
		fpExpr(b, n.List)
		fpExpr(b, n.Filter)
		fpExpr(b, n.Map)
	case *PatternComp:
		b.WriteString("pc")
		fpPattern(b, n.Pattern)
		fpWhere(b, n.Where)
		fpExpr(b, n.Proj)
	case *Index:
		b.WriteString("ix")
		fpExpr(b, n.Base)
		fpExpr(b, n.Idx)
	case *Slice:
		b.WriteString("sl")
		fpExpr(b, n.Base)
		fpExpr(b, n.From)
		fpExpr(b, n.To)
	case *PropOf:
		b.WriteString("po")
		fpExpr(b, n.Base)
		b.WriteString(strconv.Quote(n.Key))
	case *MapProj:
		b.WriteString("mp")
		b.WriteString(strconv.Quote(n.Var))
		for _, en := range n.Entries {
			b.WriteString(strconv.Itoa(int(en.Kind)))
			b.WriteString(strconv.Quote(en.Key))
			fpExpr(b, en.Expr)
		}
		b.WriteByte('.')
	case *MapLit:
		b.WriteString("ml")
		for _, f := range n.Fields {
			b.WriteString(strconv.Quote(f.Key))
			fpExpr(b, f.Val)
		}
		b.WriteByte('.')
	case *HasLabelExpr:
		b.WriteString("hl")
		b.WriteString(strconv.Quote(n.Var))
		fpLabelExpr(b, n.Expr)
	default:
		b.WriteString("?")
	}
}
