// CALL statement parsing: braced subqueries with the GQL variable-scope
// clause, and procedure calls with expression arguments and YIELD.
package parser

import (
	"math"
	"strconv"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// parseCall dispatches the three CALL forms: CALL { body }, CALL (vars) {
// body }, and CALL proc(args) YIELD items.
func (p *parser) parseCall() (ast.Clause, error) {
	p.i++ // CALL
	switch p.peek().Kind {
	case TokLBrace:
		return p.parseCallSubquery(nil)
	case TokLParen:
		p.i++
		var imports []string
		for p.peek().Kind != TokRParen {
			name, err := p.identName("an imported variable")
			if err != nil {
				return nil, err
			}
			imports = append(imports, name)
			if !p.acceptTok(TokComma) {
				break
			}
		}
		if _, err := p.expect(TokRParen, "')'"); err != nil {
			return nil, err
		}
		return p.parseCallSubquery(imports)
	default:
		return p.parseCallProc()
	}
}

// parseCallSubquery parses { body } where body is part (UNION part)*.
// Scope-clause imports synthesize a leading importing With in every branch
// (the clause shape the Rust binder consumes).
func (p *parser) parseCallSubquery(imports []string) (ast.Clause, error) {
	if _, err := p.expect(TokLBrace, "'{'"); err != nil {
		return nil, err
	}
	var q ast.Query
	part, err := p.parsePart()
	if err != nil {
		return nil, err
	}
	q.Parts = append(q.Parts, *part)
	for p.acceptKw("union") {
		kind := ast.UnionDistinct
		if p.acceptKw("all") {
			kind = ast.UnionAll
		}
		next, perr := p.parsePart()
		if perr != nil {
			return nil, perr
		}
		q.Union = append(q.Union, kind)
		q.Parts = append(q.Parts, *next)
	}
	if _, err := p.expect(TokRBrace, "'}'"); err != nil {
		return nil, err
	}
	if len(imports) > 0 {
		items := make([]ast.ReturnItem, len(imports))
		for i, v := range imports {
			items[i] = ast.ReturnItem{Expr: &ast.Var{Name: v}}
		}
		for pi := range q.Parts {
			importWith := &ast.With{Proj: ast.Projection{Items: items}}
			q.Parts[pi].Clauses = append([]ast.Clause{importWith}, q.Parts[pi].Clauses...)
		}
	}
	return &ast.CallSubquery{Query: q, Imports: imports}, nil
}

// parseCallProc is: CALL name[.name]*(args) YIELD field [AS alias][, ...].
// Args are general expressions; constant ones resolve at plan time and the
// rest evaluate per input row (a correlated call).
func (p *parser) parseCallProc() (ast.Clause, error) {
	name, err := p.identName("a procedure name")
	if err != nil {
		return nil, err
	}
	proc := name
	for p.peek().Kind == TokDot {
		p.i++
		part, perr := p.identName("a procedure name segment")
		if perr != nil {
			return nil, perr
		}
		proc += "." + part
	}
	if _, err := p.expect(TokLParen, "'('"); err != nil {
		return nil, err
	}
	var args []ast.Expr
	for p.peek().Kind != TokRParen {
		arg, aerr := p.parseExpr()
		if aerr != nil {
			return nil, aerr
		}
		args = append(args, arg)
		if !p.acceptTok(TokComma) {
			break
		}
	}
	if _, err := p.expect(TokRParen, "')'"); err != nil {
		return nil, err
	}
	if kerr := p.expectKw("yield"); kerr != nil {
		return nil, kerr
	}
	var yields []ast.YieldItem
	for {
		field, ferr := p.identName("a YIELD field")
		if ferr != nil {
			return nil, ferr
		}
		item := ast.YieldItem{Field: field}
		if p.acceptKw("as") {
			alias, aerr := p.identName("an alias")
			if aerr != nil {
				return nil, aerr
			}
			item.Alias = alias
		}
		yields = append(yields, item)
		if !p.acceptTok(TokComma) {
			break
		}
	}
	return &ast.CallProc{Proc: proc, Args: args, Yields: yields}, nil
}

// parseLiteralTok reads one literal token (int, float, string, true,
// false, null, $param).
func (p *parser) parseLiteralTok() (ast.Literal, *Error) {
	t := p.peek()
	switch {
	case t.Kind == TokInt:
		p.i++
		n, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			// 2^63 is MinInt64's magnitude: it overflows as a positive but
			// is valid under a unary minus. Read it as MinInt64 so the
			// unary-minus fold in the Pratt parser produces MinInt64
			// instead of re-overflowing (mirrors the Rust parser exactly).
			if t.Text == "9223372036854775808" {
				return ast.IntLit(math.MinInt64), nil
			}
			return ast.Literal{}, errf(t.Pos, "bad integer %q", t.Text)
		}
		return ast.IntLit(n), nil
	case t.Kind == TokFloat:
		p.i++
		f, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return ast.Literal{}, errf(t.Pos, "bad float %q", t.Text)
		}
		return ast.FloatLit(f), nil
	case t.Kind == TokStr:
		p.i++
		return ast.StrLit(t.Text), nil
	case t.Kind == TokParam:
		p.i++
		return ast.NamedParamLit(t.Text), nil
	case kwIs(t, "true"):
		p.i++
		return ast.BoolLit(true), nil
	case kwIs(t, "false"):
		p.i++
		return ast.BoolLit(false), nil
	case kwIs(t, "null"):
		p.i++
		return ast.NullLit(), nil
	}
	return ast.Literal{}, errf(t.Pos, "expected a literal, found %q", t.Text)
}
