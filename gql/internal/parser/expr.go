// The Pratt expression parser. Precedence, loosest to tightest (the Rust
// engine's table exactly): OR < AND < NOT < comparisons / IN / STARTS WITH
// / ENDS WITH / CONTAINS < + - < * / < unary - < postfix (IS [NOT] NULL,
// slice, index, .prop, :Label). NOT binds looser than comparisons, so
// NOT a = b reads NOT (a = b).
package parser

import (
	"math"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// Binding powers; an infix binds into the left operand while its power
// exceeds the context minimum (left associativity).
const (
	bpOr  = 1
	bpAnd = 2
	bpNot = 3
	bpCmp = 4
	bpAdd = 5
	bpMul = 6
	bpNeg = 7
)

// parseExpr parses one full expression.
func (p *parser) parseExpr() (ast.Expr, error) {
	return p.parseBP(0)
}

// parseBP parses an expression whose infix operators all bind tighter
// than minBP.
func (p *parser) parseBP(minBP int) (ast.Expr, error) {
	var lhs ast.Expr
	var err error
	switch {
	case p.peekKw("not"):
		p.i++
		operand, oerr := p.parseBP(bpNot - 1)
		if oerr != nil {
			return nil, oerr
		}
		lhs = &ast.Unary{Op: ast.Not, Expr: operand}
	case p.peek().Kind == TokMinus:
		p.i++
		operand, oerr := p.parseBP(bpNeg - 1)
		if oerr != nil {
			return nil, oerr
		}
		// Fold a unary minus over the MinInt64 magnitude into the literal
		// so it doesn't re-overflow at eval; every other negation stays a
		// normal Unary (mirrors the Rust parser).
		if lit, ok := operand.(*ast.Lit); ok && lit.Value.Kind == ast.LitInt && lit.Value.I == math.MinInt64 {
			lhs = lit
		} else {
			lhs = &ast.Unary{Op: ast.Neg, Expr: operand}
		}
	default:
		lhs, err = p.parsePrimary()
		if err != nil {
			return nil, err
		}
		lhs, err = p.parsePostfix(lhs)
		if err != nil {
			return nil, err
		}
	}
	for {
		op, bp, width, isIn, ok := p.peekInfix()
		if !ok || bp <= minBP {
			return lhs, nil
		}
		p.i += width
		rhs, rerr := p.parseBP(bp)
		if rerr != nil {
			return nil, rerr
		}
		if isIn {
			lhs = &ast.In{Expr: lhs, List: rhs}
		} else {
			lhs = &ast.Binary{Op: op, LHS: lhs, RHS: rhs}
		}
	}
}

// peekInfix classifies the next token(s) as an infix operator: its BinOp,
// binding power, token width (2 for STARTS WITH / ENDS WITH), and whether
// it is the IN membership operator.
func (p *parser) peekInfix() (op ast.BinOp, bp, width int, isIn, ok bool) {
	t := p.peek()
	switch t.Kind {
	case TokIdent:
		switch strings.ToLower(t.Text) {
		case "or":
			return ast.OpOr, bpOr, 1, false, true
		case "and":
			return ast.OpAnd, bpAnd, 1, false, true
		case "in":
			return 0, bpCmp, 1, true, true
		case "starts":
			if kwIs(p.peekAt(1), "with") {
				return ast.OpStartsWith, bpCmp, 2, false, true
			}
		case "ends":
			if kwIs(p.peekAt(1), "with") {
				return ast.OpEndsWith, bpCmp, 2, false, true
			}
		case "contains":
			return ast.OpContains, bpCmp, 1, false, true
		}
	case TokEq:
		return ast.OpEq, bpCmp, 1, false, true
	case TokNeq:
		return ast.OpNeq, bpCmp, 1, false, true
	case TokLt:
		return ast.OpLt, bpCmp, 1, false, true
	case TokLte:
		return ast.OpLte, bpCmp, 1, false, true
	case TokGt:
		return ast.OpGt, bpCmp, 1, false, true
	case TokGte:
		return ast.OpGte, bpCmp, 1, false, true
	case TokPlus:
		return ast.OpAdd, bpAdd, 1, false, true
	case TokMinus:
		return ast.OpSub, bpAdd, 1, false, true
	case TokStar:
		return ast.OpMul, bpMul, 1, false, true
	case TokSlash:
		return ast.OpDiv, bpMul, 1, false, true
	}
	return 0, 0, 0, false, false
}

// parsePostfix applies the postfix operators to a parsed primary: IS [NOT]
// NULL, [index], [from..to] slices, .prop, and the :Label predicate. A '{'
// here is a map projection, which is not in the GQL subset.
func (p *parser) parsePostfix(lhs ast.Expr) (ast.Expr, error) {
	for {
		t := p.peek()
		switch {
		case kwIs(t, "is"):
			p.i++
			negated := p.acceptKw("not")
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			lhs = &ast.IsNull{Expr: lhs, Negated: negated}
		case t.Kind == TokLBracket:
			var err error
			lhs, err = p.parseIndexOrSlice(lhs)
			if err != nil {
				return nil, err
			}
		case t.Kind == TokDot:
			p.i++
			key, err := p.identName("a property key")
			if err != nil {
				return nil, err
			}
			if v, isVar := lhs.(*ast.Var); isVar {
				lhs = &ast.Prop{Var: v.Name, Key: key}
			} else {
				lhs = &ast.PropOf{Base: lhs, Key: key}
			}
		case t.Kind == TokColon:
			p.i++
			le, err := p.parseLabelOr()
			if err != nil {
				return nil, err
			}
			v, isVar := lhs.(*ast.Var)
			if !isVar {
				return nil, errf(t.Pos, "label predicate ':' must apply to a variable (e.g. n:Label)")
			}
			lhs = &ast.HasLabelExpr{Var: v.Name, Expr: le}
		case t.Kind == TokLBrace:
			return nil, errf(t.Pos, "map projections (var{.key}) are not in the GQL subset: project properties explicitly")
		default:
			return lhs, nil
		}
	}
}

// parseIndexOrSlice parses base[index] or base[from..to] (either slice
// bound optional).
func (p *parser) parseIndexOrSlice(base ast.Expr) (ast.Expr, error) {
	p.i++ // '['
	// A leading '..' is a from-less slice.
	if p.acceptTok(TokDotDot) {
		return p.finishSlice(base, nil)
	}
	first, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.acceptTok(TokDotDot) {
		return p.finishSlice(base, first)
	}
	if _, err := p.expect(TokRBracket, "']' closing an index"); err != nil {
		return nil, err
	}
	return &ast.Index{Base: base, Idx: first}, nil
}

// finishSlice parses the optional upper bound and the closing bracket.
func (p *parser) finishSlice(base ast.Expr, from ast.Expr) (ast.Expr, error) {
	var to ast.Expr
	if p.peek().Kind != TokRBracket {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		to = e
	}
	if _, err := p.expect(TokRBracket, "']' closing a slice"); err != nil {
		return nil, err
	}
	return &ast.Slice{Base: base, From: from, To: to}, nil
}
