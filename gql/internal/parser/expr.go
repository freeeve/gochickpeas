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
	bpXor = 2
	bpAnd = 3
	bpNot = 4
	bpCmp = 5
	bpAdd = 6
	bpMul = 7
	bpNeg = 8
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
		lhs = foldNeg(operand)
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
		isMod := p.peek().Kind == TokPercent
		p.i += width
		rhs, rerr := p.parseBP(bp)
		if rerr != nil {
			return nil, rerr
		}
		switch {
		case isMod:
			// `a % b` is parse-time sugar for mod(a, b) -- one evaluation
			// path, multiplicative precedence.
			lhs = &ast.Func{Name: "mod", Args: []ast.Expr{lhs, rhs}}
		case isIn:
			lhs = &ast.In{Expr: lhs, List: rhs}
		default:
			lhs = &ast.Binary{Op: op, LHS: lhs, RHS: rhs}
		}
	}
}

// foldNeg folds a unary minus over a numeric literal into the literal, so
// the constant-matching paths see a negative value as one Lit rather than a
// Unary they decline: an inline prop {balance: -50} classifies as a Props
// seek instead of a desugared post-scan filter, and a WHERE bound a.x < -5
// auto-parameterizes into the shared plan template. Non-literal operands
// (-a.x, -(a + 1), -$p) stay a runtime Unary. The MinInt64 magnitude also
// stays folded to its recovered literal rather than re-negated: -(2^63) is
// MinInt64 exactly, and negating MinInt64 would overflow, so it is left to
// the overflow-to-null eval policy (mirrors the Rust parser).
func foldNeg(operand ast.Expr) ast.Expr {
	lit, ok := operand.(*ast.Lit)
	if !ok {
		return &ast.Unary{Op: ast.Neg, Expr: operand}
	}
	switch lit.Value.Kind {
	case ast.LitInt:
		if lit.Value.I == math.MinInt64 {
			return lit
		}
		return &ast.Lit{Value: ast.IntLit(-lit.Value.I)}
	case ast.LitFloat:
		return &ast.Lit{Value: ast.FloatLit(-lit.Value.F)}
	default:
		return &ast.Unary{Op: ast.Neg, Expr: operand}
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
		case "xor":
			return ast.OpXor, bpXor, 1, false, true
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
	case TokPercent:
		return ast.OpMul, bpMul, 1, false, true // op unused: %% builds mod(a,b)
	case TokPipe:
		if p.peekAt(1).Kind == TokPipe {
			return ast.OpConcat, bpAdd, 2, false, true
		}
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
			if p.acceptKw("labeled") {
				// IS [NOT] LABELED <labelexpr> -- the GQL spelling of the
				// label predicate; desugars to the ':' postfix form.
				le, lerr := p.parseLabelOr()
				if lerr != nil {
					return nil, lerr
				}
				v, isVar := lhs.(*ast.Var)
				if !isVar {
					return nil, errf(t.Pos, "IS LABELED must apply to a variable (e.g. n IS LABELED Comment)")
				}
				var e ast.Expr = &ast.HasLabelExpr{Var: v.Name, Expr: le}
				if negated {
					e = &ast.Unary{Op: ast.Not, Expr: e}
				}
				lhs = e
				continue
			}
			if p.acceptKw("true") {
				lhs = &ast.IsTruth{Expr: lhs, Want: true, Negated: negated}
				continue
			}
			if p.acceptKw("false") {
				lhs = &ast.IsTruth{Expr: lhs, Want: false, Negated: negated}
				continue
			}
			if p.acceptKw("unknown") {
				// UNKNOWN is the null truth value: IS UNKNOWN == IS NULL.
				lhs = &ast.IsNull{Expr: lhs, Negated: negated}
				continue
			}
			if p.acceptKw("typed") {
				kind, kerr := p.identName("a type name after IS TYPED")
				if kerr != nil {
					return nil, kerr
				}
				k := strings.ToLower(kind)
				switch k {
				case "int", "integer", "bigint":
					k = "integer"
				case "float", "double":
					k = "float"
				case "string", "varchar":
					k = "string"
				case "bool", "boolean":
					k = "boolean"
				case "list", "array":
					k = "list"
				case "node", "vertex":
					k = "node"
				case "relationship", "edge":
					k = "relationship"
				default:
					return nil, errf(p.peek().Pos, "IS TYPED %s is not supported (INTEGER, FLOAT, STRING, BOOLEAN, LIST, NODE, RELATIONSHIP)", kind)
				}
				lhs = &ast.IsTyped{Expr: lhs, Kind: k, Negated: negated}
				continue
			}
			if !p.acceptKw("null") {
				return nil, errf(p.peek().Pos, "expected NULL, TRUE, FALSE, UNKNOWN, TYPED, or LABELED after IS")
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
