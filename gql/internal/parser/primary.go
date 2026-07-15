// Primary expressions: literals, parens, lists, map literals, CASE,
// EXISTS/COUNT subqueries, list-predicate quantifiers, function calls, and
// variables. Ambiguity ordering mirrors the Rust grammar: COUNT/EXISTS
// followed by '{' are subqueries (count(...) stays the aggregate); the
// quantifiers all/any/none/single with a 'var IN' head are list
// predicates, otherwise plain function calls. Excluded surface
// (comprehensions, reduce, map projections, Cypher path-search functions)
// gets targeted errors.
package parser

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// parsePrimary parses one primary expression (no prefix/infix/postfix).
func (p *parser) parsePrimary() (ast.Expr, error) {
	// VALUE { ... } is the ISO scalar subquery -- not yet supported, but
	// it must say so rather than misparse into the map-projection error.
	if p.peekKw("value") && p.peekAt(1).Kind == TokLBrace {
		return nil, errf(p.peek().Pos, "VALUE { ... } scalar subqueries are not supported")
	}
	t := p.peek()
	switch t.Kind {
	case TokLParen:
		p.i++
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, perr := p.expect(TokRParen, "')'"); perr != nil {
			return nil, perr
		}
		return e, nil
	case TokLBracket:
		return p.parseListLit()
	case TokLBrace:
		return p.parseMapLit()
	case TokInt, TokFloat, TokStr, TokParam:
		lit, err := p.parseLiteralTok()
		if err != nil {
			return nil, err
		}
		return &ast.Lit{Value: lit}, nil
	case TokIdent:
		return p.parseIdentPrimary()
	}
	return nil, errf(t.Pos, "expected an expression, found %q", t.Text)
}

// parseIdentPrimary handles every identifier-led primary.
func (p *parser) parseIdentPrimary() (ast.Expr, error) {
	t := p.peek()
	// Fold the identifier into a stack buffer for the keyword dispatch: a
	// plain variable or column reference (the common case) matches no keyword
	// and is not reserved, so it never needs a heap-allocated lowercased copy
	// -- the switch and reserved probe both run over the non-copying
	// string(folded) view. An identifier longer than any keyword folds to
	// ok=false and falls straight through to the variable/function path.
	var fbuf [24]byte
	folded, foldOK := foldLower(t.Text, fbuf[:])
	if foldOK {
		switch string(folded) {
		case "true", "false", "null":
			lit, err := p.parseLiteralTok()
			if err != nil {
				return nil, err
			}
			return &ast.Lit{Value: lit}, nil
		case "case":
			return p.parseCase()
		case "exists":
			p.i++
			pat, where, err := p.parseBracedMatch("EXISTS")
			if err != nil {
				return nil, err
			}
			return &ast.Exists{Pattern: pat, Where: where}, nil
		case "count":
			if p.peekAt(1).Kind == TokLBrace {
				p.i++
				pat, where, err := p.parseBracedMatch("COUNT")
				if err != nil {
					return nil, err
				}
				return &ast.CountSub{Pattern: pat, Where: where}, nil
			}
		case "date", "datetime", "zoned_datetime", "localdatetime", "timestamp", "duration":
			// A temporal keyword directly followed by a string literal is the
			// GQL temporal-literal form (DATE '2024-01-01'); it lowers to the
			// matching constructor function. The function-call spelling has a
			// '(' here instead, so the forms never collide.
			if p.peekAt(1).Kind == TokStr {
				fn := string(folded)
				if fn == "datetime" || fn == "timestamp" {
					fn = "zoned_datetime"
				}
				p.i++
				lit, err := p.parseLiteralTok()
				if err != nil {
					return nil, err
				}
				return &ast.Func{Name: fn, Args: []ast.Expr{&ast.Lit{Value: lit}}}, nil
			}
		case "all", "any", "none", "single":
			// A quantifier with a `var IN` head is a list predicate; anything
			// else falls through to a plain function call.
			if p.peekAt(1).Kind == TokLParen && p.peekAt(2).Kind == TokIdent && kwIs(p.peekAt(3), "in") {
				return p.parseListPred(string(folded))
			}
		case "cast":
			if p.peekAt(1).Kind == TokLParen {
				return p.parseCast()
			}
		case "reduce":
			return nil, errf(t.Pos, "reduce(...) is not in the GQL subset")
		case "shortestpath", "allshortestpaths", "weightedshortestpath":
			return nil, errf(t.Pos, "%s(...) is not GQL: write MATCH p = ANY SHORTEST / ALL SHORTEST <pattern> [COST <expr>]", t.Text)
		case "cost":
			if p.peekAt(1).Kind == TokLParen {
				return nil, errf(t.Pos, "cost(shortestPath(...)) is not in the GQL subset: write MATCH p = ANY SHORTEST <pattern> COST <expr>")
			}
		}
		if reserved[string(folded)] {
			return nil, errf(t.Pos, "reserved word %q cannot start an expression", t.Text)
		}
	}
	if p.peekAt(1).Kind == TokLParen {
		return p.parseFuncCall()
	}
	p.i++
	return &ast.Var{Name: t.Text}, nil
}

// parseListLit parses a list literal, or a list comprehension
// [x IN xs [WHERE pred] [| map]] -- an extension with no ISO GQL
// spelling, matching the Rust grammar's ordered choice: a leading
// `ident IN` always opens a comprehension (parenthesize a membership
// test to keep it a literal element).
func (p *parser) parseListLit() (ast.Expr, error) {
	p.i++ // '['
	if t := p.peek(); t.Kind == TokIdent && !isReserved(t.Text) && kwIs(p.peekAt(1), "in") {
		return p.parseListComp()
	}
	list := &ast.ListExpr{}
	// A '(' opening the list is how a pattern comprehension presents when
	// its pattern cannot parse as an expression (e.g. [(a)-[:R]-(b) | x]),
	// so an element parse failure there gets the targeted rejection.
	patternish := p.peek().Kind == TokLParen
	for p.peek().Kind != TokRBracket {
		e, err := p.parseExpr()
		if err != nil {
			if patternish {
				return nil, patternCompErr(p.peek().Pos)
			}
			return nil, err
		}
		if p.peekKw("where") || p.peek().Kind == TokPipe {
			return nil, patternCompErr(p.peek().Pos)
		}
		list.Elems = append(list.Elems, e)
		if !p.acceptTok(TokComma) {
			break
		}
	}
	if _, err := p.expect(TokRBracket, "']' closing a list"); err != nil {
		return nil, err
	}
	return list, nil
}

// patternCompErr is the targeted pattern-comprehension rejection with its
// GQL-conformant rewrites.
func patternCompErr(pos int) *Error {
	return errf(pos, "pattern comprehensions are not in the GQL subset: rewrite size([pat | x]) as COUNT { MATCH pat } and value collection as MATCH pat + collect(...)")
}

// parseListComp parses the comprehension body after '[': var IN list
// [WHERE pred] ['|' map] ']'.
func (p *parser) parseListComp() (ast.Expr, error) {
	name, err := p.identName("a comprehension variable")
	if err != nil {
		return nil, err
	}
	if kerr := p.expectKw("in"); kerr != nil {
		return nil, kerr
	}
	list, lerr := p.parseExpr()
	if lerr != nil {
		return nil, lerr
	}
	lc := &ast.ListComp{Var: name, List: list}
	if p.acceptKw("where") {
		f, ferr := p.parseExpr()
		if ferr != nil {
			return nil, ferr
		}
		lc.Filter = f
	}
	if p.acceptTok(TokPipe) {
		m, merr := p.parseExpr()
		if merr != nil {
			return nil, merr
		}
		lc.Map = m
	}
	if _, err := p.expect(TokRBracket, "']' closing a comprehension"); err != nil {
		return nil, err
	}
	return lc, nil
}

// parseCast parses CAST(expr AS TYPE) -- the GQL cast spelling, lowered
// to the matching conversion function.
func (p *parser) parseCast() (ast.Expr, error) {
	p.i += 2 // 'cast' '('
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if kerr := p.expectKw("as"); kerr != nil {
		return nil, kerr
	}
	t, terr := p.expect(TokIdent, "a CAST target type")
	if terr != nil {
		return nil, terr
	}
	var fbuf [16]byte
	folded, _ := foldLower(t.Text, fbuf[:])
	var fn string
	switch string(folded) {
	case "float", "double":
		fn = "tofloat"
	case "int", "integer", "bigint":
		fn = "tointeger"
	case "string", "varchar":
		fn = "tostring"
	case "bool", "boolean":
		fn = "toboolean"
	case "date":
		fn = "date"
	case "datetime", "zoned_datetime", "timestamp":
		fn = "zoned_datetime"
	case "localdatetime":
		fn = "localdatetime"
	case "duration":
		fn = "duration"
	default:
		return nil, errf(t.Pos, "CAST target %q is not supported (FLOAT, INTEGER, STRING, BOOLEAN, DATE, DATETIME, DURATION)", t.Text)
	}
	if _, err := p.expect(TokRParen, "')' closing the CAST"); err != nil {
		return nil, err
	}
	return &ast.Func{Name: fn, Args: []ast.Expr{e}}, nil
}

// parseMapLit parses {key: expr, ...} in expression position.
func (p *parser) parseMapLit() (ast.Expr, error) {
	p.i++ // '{'
	m := &ast.MapLit{}
	for p.peek().Kind != TokRBrace {
		key, err := p.identName("a map key")
		if err != nil {
			return nil, err
		}
		if _, cerr := p.expect(TokColon, "':'"); cerr != nil {
			return nil, cerr
		}
		e, eerr := p.parseExpr()
		if eerr != nil {
			return nil, eerr
		}
		m.Fields = append(m.Fields, ast.MapField{Key: key, Val: e})
		if !p.acceptTok(TokComma) {
			break
		}
	}
	if _, err := p.expect(TokRBrace, "'}' closing a map"); err != nil {
		return nil, err
	}
	return m, nil
}

// parseCase parses CASE [operand] (WHEN cond THEN result)+ [ELSE default]
// END; without an operand it is the searched form.
func (p *parser) parseCase() (ast.Expr, error) {
	p.i++ // CASE
	c := &ast.Case{}
	if !p.peekKw("when") {
		operand, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Operand = operand
	}
	for p.acceptKw("when") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if kerr := p.expectKw("then"); kerr != nil {
			return nil, kerr
		}
		result, rerr := p.parseExpr()
		if rerr != nil {
			return nil, rerr
		}
		c.Whens = append(c.Whens, ast.CaseWhen{Cond: cond, Result: result})
	}
	if len(c.Whens) == 0 {
		return nil, errf(p.peek().Pos, "CASE needs at least one WHEN")
	}
	if p.acceptKw("else") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Else = e
	}
	if err := p.expectKw("end"); err != nil {
		return nil, err
	}
	return c, nil
}

// parseBracedMatch parses { [MATCH] pattern [WHERE expr] } -- the body of
// EXISTS / COUNT subqueries. The MATCH keyword is optional (GQL's bare
// EXISTS { <pattern> } spelling).
func (p *parser) parseBracedMatch(what string) (*ast.Pattern, ast.Expr, error) {
	if _, err := p.expect(TokLBrace, "'{' after "+what); err != nil {
		return nil, nil, err
	}
	p.acceptKw("match")
	pat, perr := p.parsePattern()
	if perr != nil {
		return nil, nil, perr
	}
	where, werr := p.parseOptionalWhere()
	if werr != nil {
		return nil, nil, werr
	}
	if _, err := p.expect(TokRBrace, "'}' closing "+what); err != nil {
		return nil, nil, err
	}
	return pat, where, nil
}

// parseListPred parses quant(var IN list WHERE pred).
func (p *parser) parseListPred(kw string) (ast.Expr, error) {
	var quant ast.Quant
	switch kw {
	case "all":
		quant = ast.QuantAll
	case "any":
		quant = ast.QuantAny
	case "none":
		quant = ast.QuantNone
	default:
		quant = ast.QuantSingle
	}
	p.i += 2 // quant '('
	name, err := p.identName("a quantifier variable")
	if err != nil {
		return nil, err
	}
	if kerr := p.expectKw("in"); kerr != nil {
		return nil, kerr
	}
	list, lerr := p.parseExpr()
	if lerr != nil {
		return nil, lerr
	}
	if kerr := p.expectKw("where"); kerr != nil {
		return nil, kerr
	}
	pred, perr := p.parseExpr()
	if perr != nil {
		return nil, perr
	}
	if _, err := p.expect(TokRParen, "')' closing the quantifier"); err != nil {
		return nil, err
	}
	return &ast.ListPred{Quant: quant, Var: name, List: list, Pred: pred}, nil
}

// parseFuncCall parses name([DISTINCT] * | args...).
func (p *parser) parseFuncCall() (ast.Expr, error) {
	name, err := p.identName("a function name")
	if err != nil {
		return nil, err
	}
	p.i++ // '('
	f := &ast.Func{Name: name}
	f.Distinct = p.acceptKw("distinct")
	if p.acceptTok(TokStar) {
		f.Star = true
	} else {
		for p.peek().Kind != TokRParen {
			arg, aerr := p.parseExpr()
			if aerr != nil {
				return nil, aerr
			}
			f.Args = append(f.Args, arg)
			if !p.acceptTok(TokComma) {
				break
			}
		}
	}
	if _, err := p.expect(TokRParen, "')' closing the call"); err != nil {
		return nil, err
	}
	return f, nil
}
