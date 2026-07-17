// Expression and literal rendering for the plan tree (split from
// render.go for the file-size norm).
package explain

import (
	"fmt"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

func fmtAgg(a *plan.AggCol) string {
	kind := [...]string{"count", "sum", "avg", "min", "max", "collect",
		"stddev_samp", "stddev_pop", "percentile_cont", "percentile_disc"}[a.Kind]
	inner := "*"
	if a.Arg != nil {
		inner = fmtExpr(a.Arg)
	}
	d := ""
	if a.Distinct {
		d = "DISTINCT "
	}
	return kind + "(" + d + inner + ")"
}

func fmtSort(s ast.SortItem) string {
	if s.Desc {
		return fmtExpr(s.Expr) + " DESC"
	}
	return fmtExpr(s.Expr)
}

func fmtLit(l ast.Literal) string {
	switch l.Kind {
	case ast.LitInt:
		return fmt.Sprintf("%d", l.I)
	case ast.LitFloat:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", l.F), "0"), ".")
	case ast.LitStr:
		return "'" + l.S + "'"
	case ast.LitBool:
		return fmt.Sprintf("%t", l.B)
	case ast.LitNull:
		return "null"
	case ast.LitParam:
		return fmt.Sprintf("$auto%d", l.P)
	default:
		return "$" + l.S
	}
}

func fmtExpr(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Lit:
		return fmtLit(n.Value)
	case *ast.Var:
		return n.Name
	case *ast.Prop:
		return n.Var + "." + n.Key
	case *ast.Unary:
		if n.Op == ast.Not {
			return "NOT " + fmtExpr(n.Expr)
		}
		return "-" + fmtExpr(n.Expr)
	case *ast.Binary:
		return fmtExpr(n.LHS) + " " + binopStr(n.Op) + " " + fmtExpr(n.RHS)
	case *ast.Func:
		a := ""
		if n.Star {
			a = "*"
		} else {
			parts := make([]string, len(n.Args))
			for i, x := range n.Args {
				parts[i] = fmtExpr(x)
			}
			a = strings.Join(parts, ", ")
		}
		d := ""
		if n.Distinct {
			d = "DISTINCT "
		}
		return n.Name + "(" + d + a + ")"
	case *ast.ListExpr:
		parts := make([]string, len(n.Elems))
		for i, x := range n.Elems {
			parts[i] = fmtExpr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *ast.In:
		return fmtExpr(n.Expr) + " IN " + fmtExpr(n.List)
	case *ast.IsNull:
		if n.Negated {
			return fmtExpr(n.Expr) + " IS NOT NULL"
		}
		return fmtExpr(n.Expr) + " IS NULL"
	case *ast.IsTruth:
		s := " IS TRUE"
		if !n.Want {
			s = " IS FALSE"
		}
		if n.Negated {
			s = " IS NOT" + s[3:]
		}
		return fmtExpr(n.Expr) + s
	case *ast.IsTyped:
		s := " IS TYPED "
		if n.Negated {
			s = " IS NOT TYPED "
		}
		return fmtExpr(n.Expr) + s + n.Kind
	case *ast.Case:
		return "CASE…END"
	case *ast.Cost:
		return fmt.Sprintf("cost(shortestPath((%s)..(%s)), …)", n.From, n.To)
	case *ast.PatternComp:
		return "[(…) | " + fmtExpr(n.Proj) + "]"
	case *ast.Exists:
		return "EXISTS {…}"
	case *ast.CountSub:
		return "COUNT {…}"
	case *ast.ListPred:
		q := [...]string{"all", "any", "none", "single"}[n.Quant]
		return fmt.Sprintf("%s(%s IN %s WHERE …)", q, n.Var, fmtExpr(n.List))
	case *ast.Reduce:
		return fmt.Sprintf("reduce(%s = %s, %s IN %s | …)", n.Acc, fmtExpr(n.Init), n.Var, fmtExpr(n.List))
	case *ast.ListComp:
		return fmt.Sprintf("[%s IN %s …]", n.Var, fmtExpr(n.List))
	case *ast.Index:
		return fmtExpr(n.Base) + "[" + fmtExpr(n.Idx) + "]"
	case *ast.Slice:
		from, to := "", ""
		if n.From != nil {
			from = fmtExpr(n.From)
		}
		if n.To != nil {
			to = fmtExpr(n.To)
		}
		return fmtExpr(n.Base) + "[" + from + ".." + to + "]"
	case *ast.PropOf:
		return fmtExpr(n.Base) + "." + n.Key
	case *ast.MapProj:
		parts := make([]string, len(n.Entries))
		for i, en := range n.Entries {
			switch en.Kind {
			case ast.MapProjProp:
				parts[i] = "." + en.Key
			case ast.MapProjAll:
				parts[i] = ".*"
			default:
				parts[i] = en.Key + ": " + fmtExpr(en.Expr)
			}
		}
		return n.Var + "{" + strings.Join(parts, ", ") + "}"
	case *ast.MapLit:
		parts := make([]string, len(n.Fields))
		for i, f := range n.Fields {
			parts[i] = f.Key + ": " + fmtExpr(f.Val)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *ast.HasLabelExpr:
		return n.Var + ":" + fmtLabelExpr(n.Expr)
	}
	return "?"
}

// fmtLabelExpr renders a label expression, parenthesizing &/| so
// precedence is unambiguous.
func fmtLabelExpr(e *ast.LabelExpr) string {
	switch e.Kind {
	case ast.LabelName:
		return e.Name
	case ast.LabelWild:
		return "%"
	case ast.LabelAnd:
		return "(" + fmtLabelExpr(e.L) + "&" + fmtLabelExpr(e.R) + ")"
	case ast.LabelOr:
		return "(" + fmtLabelExpr(e.L) + "|" + fmtLabelExpr(e.R) + ")"
	default:
		return "!" + fmtLabelExpr(e.L)
	}
}

func binopStr(op ast.BinOp) string {
	switch op {
	case ast.OpOr:
		return "OR"
	case ast.OpAnd:
		return "AND"
	case ast.OpEq:
		return "="
	case ast.OpNeq:
		return "<>"
	case ast.OpLt:
		return "<"
	case ast.OpLte:
		return "<="
	case ast.OpGt:
		return ">"
	case ast.OpGte:
		return ">="
	case ast.OpAdd:
		return "+"
	case ast.OpSub:
		return "-"
	case ast.OpMul:
		return "*"
	case ast.OpDiv:
		return "/"
	case ast.OpStartsWith:
		return "STARTS WITH"
	case ast.OpEndsWith:
		return "ENDS WITH"
	default:
		return "CONTAINS"
	}
}
