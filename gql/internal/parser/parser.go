// Package parser is the hand-written GQL parser (lexer + recursive descent
// + Pratt expressions): the read-only ISO GQL subset specified in
// gql/GRAMMAR.md, lowered into the language-neutral AST.
//
// GQL surface forms normalize into the Rust engine's segment model:
// RETURN ... NEXT becomes an ast.With projection boundary; LET x = e
// becomes With{star + items}; FILTER pred becomes With{star, where}; FOR x
// IN list becomes ast.Unwind; CALL (vars) { ... } becomes ast.CallSubquery
// with the scope vars as imports AND a synthesized importing With
// prepended to every branch of the body (the clause shape the binder
// expects). Cypher-only spellings (WITH, UNWIND, *1..3, shortestPath())
// are rejected with pointers to the GQL forms.
package parser

import (
	"strconv"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// parser holds the token stream cursor.
type parser struct {
	toks []Token
	i    int
}

// Parse parses one GQL query. The error, when non-nil, is a *parser.Error
// carrying the byte offset (the root gql package wraps it with ErrParse).
func Parse(src string) (*ast.Query, error) {
	toks, lerr := lex(src)
	if lerr != nil {
		return nil, lerr
	}
	p := &parser{toks: toks}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TokEOF {
		return nil, errf(p.peek().Pos, "unexpected trailing input %q", p.peek().Text)
	}
	return q, nil
}

func (p *parser) peek() Token { return p.toks[p.i] }
func (p *parser) peekAt(n int) Token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}
func (p *parser) next() Token { t := p.toks[p.i]; p.i++; return t }

// kwIs reports whether t is the case-insensitive keyword kw.
func kwIs(t Token, kw string) bool {
	return t.Kind == TokIdent && strings.EqualFold(t.Text, kw)
}

// peekKw reports whether the next token is the keyword kw.
func (p *parser) peekKw(kw string) bool { return kwIs(p.peek(), kw) }

// acceptKw consumes the keyword kw if it is next.
func (p *parser) acceptKw(kw string) bool {
	if p.peekKw(kw) {
		p.i++
		return true
	}
	return false
}

// expectKw consumes the keyword kw or fails.
func (p *parser) expectKw(kw string) *Error {
	if !p.acceptKw(kw) {
		return errf(p.peek().Pos, "expected %s, found %q", strings.ToUpper(kw), p.peek().Text)
	}
	return nil
}

// expect consumes a token of the given kind or fails.
func (p *parser) expect(kind TokKind, what string) (Token, *Error) {
	if p.peek().Kind != kind {
		return Token{}, errf(p.peek().Pos, "expected %s, found %q", what, p.peek().Text)
	}
	return p.next(), nil
}

// identName consumes a non-reserved identifier (a variable, label, key, or
// alias name).
func (p *parser) identName(what string) (string, *Error) {
	t := p.peek()
	if t.Kind != TokIdent {
		return "", errf(t.Pos, "expected %s, found %q", what, t.Text)
	}
	if reserved[strings.ToLower(t.Text)] {
		return "", errf(t.Pos, "reserved word %q cannot be used as %s", t.Text, what)
	}
	p.i++
	return t.Text, nil
}

// parseQuery is: [EXPLAIN|PROFILE] part (UNION [ALL] part)*.
func (p *parser) parseQuery() (*ast.Query, error) {
	q := &ast.Query{}
	if p.acceptKw("explain") {
		q.Mode = ast.Explain
	} else if p.acceptKw("profile") {
		q.Mode = ast.Profile
	}
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
		next, err := p.parsePart()
		if err != nil {
			return nil, err
		}
		q.Union = append(q.Union, kind)
		q.Parts = append(q.Parts, *next)
	}
	return q, nil
}

// parsePart is one UNION branch: statements ending in a RETURN with no
// NEXT. RETURN ... NEXT lowers to a With boundary and the part continues.
func (p *parser) parsePart() (*ast.QueryPart, error) {
	part := &ast.QueryPart{}
	for {
		t := p.peek()
		if t.Kind != TokIdent {
			return nil, errf(t.Pos, "expected a statement, found %q", t.Text)
		}
		kw := strings.ToLower(t.Text)
		if writeKeywords[kw] {
			return nil, errf(t.Pos, "%s is not supported: this is a read-only engine", strings.ToUpper(kw))
		}
		switch kw {
		case "match", "optional":
			c, err := p.parseMatch()
			if err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, c)
		case "filter":
			p.i++
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, &ast.With{Proj: ast.Projection{Star: true}, Where: e})
		case "let":
			c, err := p.parseLet()
			if err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, c)
		case "for":
			c, err := p.parseFor()
			if err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, c)
		case "call":
			c, err := p.parseCall()
			if err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, c)
		case "order":
			// Standalone ORDER BY [OFFSET] [LIMIT]: sort (and cut) the
			// binding table mid-pipeline -- a star projection carrying
			// only the ordering, the GQL analogue of Cypher's
			// `WITH * ORDER BY ... LIMIT n`.
			proj := ast.Projection{Star: true}
			if err := p.parseProjectionTail(&proj); err != nil {
				return nil, err
			}
			part.Clauses = append(part.Clauses, &ast.With{Proj: proj})
		case "return":
			p.i++
			proj, err := p.parseProjection()
			if err != nil {
				return nil, err
			}
			if p.acceptKw("next") {
				part.Clauses = append(part.Clauses, &ast.With{Proj: *proj})
				continue
			}
			part.Ret = *proj
			return part, nil
		case "next":
			// A bare NEXT between statements is the ISO statement
			// separator: the binding table flows forward unchanged, so
			// it is a no-op here (RETURN ... NEXT is the projecting
			// boundary).
			p.i++
		case "with":
			return nil, errf(t.Pos, "WITH is not GQL: use RETURN ... NEXT (projection boundary), LET (bindings), or FILTER (predicate)")
		case "unwind":
			return nil, errf(t.Pos, "UNWIND is not GQL: use FOR x IN <list>")
		default:
			return nil, errf(t.Pos, "expected a statement (MATCH, FILTER, LET, FOR, CALL, ORDER BY, RETURN), found %q", t.Text)
		}
	}
}

// parseLet is: LET x = expr [, y = expr]* -- a pass-through projection
// (star) extended with the new bindings.
func (p *parser) parseLet() (ast.Clause, error) {
	p.i++ // LET
	proj := ast.Projection{Star: true}
	for {
		name, err := p.identName("a LET binding name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(TokEq, "'='"); err != nil {
			return nil, err
		}
		e, eerr := p.parseExpr()
		if eerr != nil {
			return nil, eerr
		}
		proj.Items = append(proj.Items, ast.ReturnItem{Expr: e, Alias: name})
		if !p.acceptTok(TokComma) {
			break
		}
	}
	return &ast.With{Proj: proj}, nil
}

// parseFor is: FOR var IN expr.
func (p *parser) parseFor() (ast.Clause, error) {
	p.i++ // FOR
	name, err := p.identName("a FOR variable")
	if err != nil {
		return nil, err
	}
	if kerr := p.expectKw("in"); kerr != nil {
		return nil, kerr
	}
	e, eerr := p.parseExpr()
	if eerr != nil {
		return nil, eerr
	}
	return &ast.Unwind{Expr: e, Var: name}, nil
}

// acceptTok consumes a token of the given kind if it is next.
func (p *parser) acceptTok(kind TokKind) bool {
	if p.peek().Kind == kind {
		p.i++
		return true
	}
	return false
}

// parseMatch is: [OPTIONAL] MATCH [mode] <body> [WHERE expr]. The body is
// either comma-separated patterns, a path bind `p = [mode] <pattern>`, or
// a path search `p = ANY|ALL SHORTEST <pattern>`; mode is a path-mode
// prefix (TRAIL / ACYCLIC -- see parsePathMode).
func (p *parser) parseMatch() (ast.Clause, error) {
	optional := p.acceptKw("optional")
	if err := p.expectKw("match"); err != nil {
		return nil, err
	}
	// Match modes: DIFFERENT EDGES is the engine default (accepted as a
	// no-op); REPEATABLE ELEMENTS switches the clause to walk semantics
	// (no relationship-uniqueness enforcement).
	repeatable := false
	switch {
	case p.peekKw("repeatable") && kwIs(p.peekAt(1), "elements"):
		p.i += 2
		repeatable = true
	case p.peekKw("different") && kwIs(p.peekAt(1), "edges"):
		p.i += 2
	}
	// `ident =` introduces a path binding (a pattern starts with '(').
	if p.peek().Kind == TokIdent && !reserved[strings.ToLower(p.peek().Text)] && p.peekAt(1).Kind == TokEq {
		pathVar, _ := p.identName("a path variable")
		p.i++ // '='
		acyclic, merr := p.parsePathMode()
		if merr != nil {
			return nil, merr
		}
		all := false
		search := false
		switch {
		case p.peekKw("any") && kwIs(p.peekAt(1), "shortest"):
			p.i += 2
			search = true
		case p.peekKw("all") && kwIs(p.peekAt(1), "shortest"):
			p.i += 2
			search, all = true, true
		case p.peekKw("shortest"):
			return nil, errf(p.peek().Pos, "bare SHORTEST is not supported: use ANY SHORTEST or ALL SHORTEST")
		case p.peekKw("shortestpath"), p.peekKw("allshortestpaths"), p.peekKw("weightedshortestpath"):
			return nil, errf(p.peek().Pos, "%s(...) is not GQL: write MATCH p = ANY SHORTEST / ALL SHORTEST <pattern> [COST <expr>]", p.peek().Text)
		}
		if acyclic && search {
			return nil, errf(p.peek().Pos, "a path mode does not combine with ANY/ALL SHORTEST (the search normalizes the mode away)")
		}
		pat, err := p.parsePattern()
		if err != nil {
			return nil, err
		}
		var weight *ast.CostSpec
		if p.peekKw("cost") {
			if !search {
				return nil, errf(p.peek().Pos, "COST applies only to a path search: MATCH p = ANY SHORTEST <pattern> COST <expr>")
			}
			p.i++
			weight, err = p.parseCostSpec()
			if err != nil {
				return nil, err
			}
		}
		where, werr := p.parseOptionalWhere()
		if werr != nil {
			return nil, werr
		}
		if search {
			return &ast.ShortestPath{PathVar: pathVar, Pattern: *pat, Optional: optional, All: all, Weight: weight, Where: where}, nil
		}
		return &ast.PathBind{PathVar: pathVar, Pattern: *pat, Optional: optional, Where: where, Acyclic: acyclic}, nil
	}
	acyclic, merr := p.parsePathMode()
	if merr != nil {
		return nil, merr
	}
	var patterns []ast.Pattern
	for {
		pat, err := p.parsePattern()
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, *pat)
		if !p.acceptTok(TokComma) {
			break
		}
	}
	where, werr := p.parseOptionalWhere()
	if werr != nil {
		return nil, werr
	}
	return &ast.Match{Patterns: patterns, Where: where, Optional: optional, Acyclic: acyclic, Repeatable: repeatable}, nil
}

// parseCostSpec parses the COST <expr> weight of a weighted path search.
// A numeric literal is a constant per-edge weight; any other expression is
// a per-edge formula (the planner narrows `rel.prop` to a property read).
func (p *parser) parseCostSpec() (*ast.CostSpec, error) {
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if lit, ok := e.(*ast.Lit); ok {
		switch lit.Value.Kind {
		case ast.LitInt:
			return &ast.CostSpec{Kind: ast.CostConstant, Const: float64(lit.Value.I)}, nil
		case ast.LitFloat:
			return &ast.CostSpec{Kind: ast.CostConstant, Const: lit.Value.F}, nil
		}
	}
	return &ast.CostSpec{Kind: ast.CostExpr, Expr: e}, nil
}

// parsePathMode consumes an optional path-mode prefix. TRAIL is accepted
// as a no-op -- relationship-unique traversal is the engine's native
// semantics; ACYCLIC additionally forbids repeated nodes within each
// quantified segment. WALK (repeats allowed) and SIMPLE are rejected with
// targeted errors.
func (p *parser) parsePathMode() (acyclic bool, err error) {
	t := p.peek()
	if t.Kind != TokIdent {
		return false, nil
	}
	switch strings.ToLower(t.Text) {
	case "trail":
		p.i++
	case "acyclic":
		p.i++
		return true, nil
	case "walk":
		return false, errf(t.Pos, "the WALK path mode is not supported: traversal is TRAIL (no repeated relationship)")
	case "simple":
		return false, errf(t.Pos, "the SIMPLE path mode is not supported (TRAIL and ACYCLIC are)")
	}
	return false, nil
}

// parseOptionalWhere parses a trailing WHERE expr if present.
func (p *parser) parseOptionalWhere() (ast.Expr, error) {
	if !p.acceptKw("where") {
		return nil, nil
	}
	return p.parseExpr()
}

// parseProjection is: [DISTINCT] item[, item]* [ORDER BY ...] [OFFSET|SKIP
// n] [LIMIT n]; an item is `*` or expr [AS alias].
func (p *parser) parseProjection() (*ast.Projection, error) {
	proj := &ast.Projection{}
	proj.Distinct = p.acceptKw("distinct")
	for {
		if p.acceptTok(TokStar) {
			proj.Star = true
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := ast.ReturnItem{Expr: e}
			if p.acceptKw("as") {
				alias, aerr := p.identName("an alias")
				if aerr != nil {
					return nil, aerr
				}
				item.Alias = alias
			}
			proj.Items = append(proj.Items, item)
		}
		if !p.acceptTok(TokComma) {
			break
		}
	}
	if err := p.parseProjectionTail(proj); err != nil {
		return nil, err
	}
	return proj, nil
}

// parseProjectionTail parses the optional ORDER BY / OFFSET / LIMIT
// suffix into proj -- shared by RETURN and the standalone ORDER BY
// statement (which sorts the binding table mid-pipeline).
func (p *parser) parseProjectionTail(proj *ast.Projection) error {
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			item := ast.SortItem{Expr: e}
			if p.acceptKw("desc") {
				item.Desc = true
			} else {
				p.acceptKw("asc")
			}
			proj.OrderBy = append(proj.OrderBy, item)
			if !p.acceptTok(TokComma) {
				break
			}
		}
	}
	if p.acceptKw("offset") || p.acceptKw("skip") {
		n, err := p.parseCount("OFFSET")
		if err != nil {
			return err
		}
		proj.Skip = &n
	}
	if p.acceptKw("limit") {
		n, err := p.parseCount("LIMIT")
		if err != nil {
			return err
		}
		proj.Limit = &n
	}
	return nil
}

// parseCount reads a non-negative integer argument of OFFSET/LIMIT.
func (p *parser) parseCount(what string) (uint64, *Error) {
	t, err := p.expect(TokInt, what+" count")
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseUint(t.Text, 10, 64)
	if perr != nil {
		return 0, errf(t.Pos, "bad %s count %q", what, t.Text)
	}
	return n, nil
}
