// Canonical identity for a decorrelated COUNT{} side table: a byte string
// over the subquery's pattern and WHERE with the anchor and group
// variables substituted by positional markers, so two sibling subqueries
// differing only in what the outer query names those endpoints (BI Q8's
// C1(person)/C1(friend)) canonicalize identically and share one table per
// anchor node. Fail-closed: any construct the encoder does not cover
// yields no identity and the node keeps a private table -- sharing is an
// optimization, never a semantic commitment. Parameters encode by slot or
// name, which is sound because tables are shared only within one
// execution's Ctx, where parameter values are fixed.
package compile

import (
	"strconv"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// decorCanon renders the canonical identity, "" when the shape has none.
// The substitution markers use bytes no identifier contains.
func decorCanon(p *ast.Pattern, where ast.Expr, anchorVar, groupVar string) string {
	var sb strings.Builder
	sub := map[string]string{anchorVar: "\x01", groupVar: "\x02"}
	if !canonPattern(&sb, p, sub) {
		return ""
	}
	sb.WriteByte('|')
	if where != nil && !canonExpr(&sb, where, sub) {
		return ""
	}
	return sb.String()
}

// canonVar writes a variable reference under the substitution map.
func canonVar(sb *strings.Builder, v string, sub map[string]string) {
	if m, ok := sub[v]; ok {
		sb.WriteString(m)
		return
	}
	sb.WriteString("v(")
	sb.WriteString(v)
	sb.WriteByte(')')
}

// canonPattern encodes a linear pattern: nodes and hops in written order.
func canonPattern(sb *strings.Builder, p *ast.Pattern, sub map[string]string) bool {
	if !canonNode(sb, &p.Start, sub) {
		return false
	}
	for i := range p.Hops {
		h := &p.Hops[i]
		if h.Rel.Length != nil {
			return false // decor never admits these; fail closed anyway
		}
		sb.WriteByte('-')
		sb.WriteByte(byte('0' + int(h.Rel.Dir)))
		canonVar(sb, h.Rel.Var, sub)
		sb.WriteString(strings.Join(h.Rel.Types, ","))
		if len(h.Rel.Props) > 0 || len(h.Rel.PropExprs) > 0 {
			return false
		}
		sb.WriteByte('-')
		if !canonNode(sb, &h.Node, sub) {
			return false
		}
	}
	return true
}

// canonNode encodes one node pattern: variable, labels, literal props.
func canonNode(sb *strings.Builder, n *ast.NodePat, sub map[string]string) bool {
	sb.WriteByte('(')
	canonVar(sb, n.Var, sub)
	if n.LabelExpr != nil {
		if !canonLabelExpr(sb, n.LabelExpr) {
			return false
		}
	} else {
		sb.WriteByte(':')
		sb.WriteString(strings.Join(n.Labels, "&"))
	}
	if len(n.PropExprs) > 0 {
		return false
	}
	for _, pe := range n.Props {
		sb.WriteByte('{')
		sb.WriteString(pe.Key)
		sb.WriteByte(':')
		canonLiteral(sb, pe.Val)
		sb.WriteByte('}')
	}
	sb.WriteByte(')')
	return true
}

// canonLabelExpr encodes a label-expression tree structurally.
func canonLabelExpr(sb *strings.Builder, e *ast.LabelExpr) bool {
	switch e.Kind {
	case ast.LabelName:
		sb.WriteString(":n(")
		sb.WriteString(e.Name)
		sb.WriteByte(')')
	case ast.LabelAnd, ast.LabelOr:
		if e.Kind == ast.LabelAnd {
			sb.WriteString(":and(")
		} else {
			sb.WriteString(":or(")
		}
		if !canonLabelExpr(sb, e.L) || !canonLabelExpr(sb, e.R) {
			return false
		}
		sb.WriteByte(')')
	case ast.LabelWild:
		sb.WriteString(":w")
	case ast.LabelNot:
		sb.WriteString(":not(")
		if !canonLabelExpr(sb, e.L) {
			return false
		}
		sb.WriteByte(')')
	default:
		return false
	}
	return true
}

// canonLiteral encodes a literal, parameters by slot/name (sound within
// one execution: parameter values are fixed per Ctx).
func canonLiteral(sb *strings.Builder, l ast.Literal) {
	switch l.Kind {
	case ast.LitInt:
		sb.WriteByte('i')
		sb.WriteString(strconv.FormatInt(l.I, 10))
	case ast.LitFloat:
		sb.WriteByte('f')
		sb.WriteString(strconv.FormatFloat(l.F, 'b', -1, 64))
	case ast.LitStr:
		sb.WriteByte('s')
		sb.WriteString(strconv.Quote(l.S))
	case ast.LitBool:
		sb.WriteByte('b')
		if l.B {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	case ast.LitParam:
		sb.WriteByte('p')
		sb.WriteString(strconv.FormatUint(uint64(l.P), 10))
	case ast.LitNamedParam:
		sb.WriteString("P(")
		sb.WriteString(l.S)
		sb.WriteByte(')')
	default:
		sb.WriteByte('n') // null
	}
}

// canonExpr encodes the WHERE subset decor-eligible subqueries use;
// anything else fails closed.
func canonExpr(sb *strings.Builder, e ast.Expr, sub map[string]string) bool {
	switch n := e.(type) {
	case *ast.Lit:
		canonLiteral(sb, n.Value)
	case *ast.Var:
		canonVar(sb, n.Name, sub)
	case *ast.Prop:
		canonVar(sb, n.Var, sub)
		sb.WriteByte('.')
		sb.WriteString(n.Key)
	case *ast.PropOf:
		sb.WriteString("of(")
		if !canonExpr(sb, n.Base, sub) {
			return false
		}
		sb.WriteByte('.')
		sb.WriteString(n.Key)
		sb.WriteByte(')')
	case *ast.Unary:
		sb.WriteByte('u')
		sb.WriteByte(byte('0' + int(n.Op)))
		if !canonExpr(sb, n.Expr, sub) {
			return false
		}
	case *ast.Binary:
		sb.WriteString("(b")
		sb.WriteString(strconv.Itoa(int(n.Op)))
		if !canonExpr(sb, n.LHS, sub) {
			return false
		}
		sb.WriteByte(',')
		if !canonExpr(sb, n.RHS, sub) {
			return false
		}
		sb.WriteByte(')')
	case *ast.IsNull:
		sb.WriteString("isnull(")
		if n.Negated {
			sb.WriteByte('!')
		}
		if !canonExpr(sb, n.Expr, sub) {
			return false
		}
		sb.WriteByte(')')
	case *ast.In:
		sb.WriteString("in(")
		if !canonExpr(sb, n.Expr, sub) {
			return false
		}
		sb.WriteByte(',')
		if !canonExpr(sb, n.List, sub) {
			return false
		}
		sb.WriteByte(')')
	case *ast.ListExpr:
		sb.WriteString("l(")
		for _, el := range n.Elems {
			if !canonExpr(sb, el, sub) {
				return false
			}
			sb.WriteByte(',')
		}
		sb.WriteByte(')')
	case *ast.Func:
		sb.WriteString("fn(")
		sb.WriteString(strings.ToLower(n.Name))
		if n.Distinct || n.Star {
			return false // never in a decor WHERE; fail closed
		}
		for _, a := range n.Args {
			sb.WriteByte(',')
			if !canonExpr(sb, a, sub) {
				return false
			}
		}
		sb.WriteByte(')')
	case *ast.Case:
		sb.WriteString("case(")
		if n.Operand != nil {
			if !canonExpr(sb, n.Operand, sub) {
				return false
			}
		}
		for _, w := range n.Whens {
			sb.WriteString(";w")
			if !canonExpr(sb, w.Cond, sub) {
				return false
			}
			sb.WriteString(";t")
			if !canonExpr(sb, w.Result, sub) {
				return false
			}
		}
		if n.Else != nil {
			sb.WriteString(";e")
			if !canonExpr(sb, n.Else, sub) {
				return false
			}
		}
		sb.WriteByte(')')
	case *ast.HasLabelExpr:
		sb.WriteString("has(")
		canonVar(sb, n.Var, sub)
		if !canonLabelExpr(sb, n.Expr) {
			return false
		}
		sb.WriteByte(')')
	default:
		return false
	}
	return true
}
