// Path-pattern parsing: node patterns with label expressions and inline
// property maps, relationship patterns with GQL postfix quantifiers
// ({m,n}, {m,}, {,n}, {m}, *, +) after the arrow -- the Cypher in-bracket
// var-length spelling (*1..3) is rejected with a pointer to the GQL form.
package parser

import (
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// parsePattern is: node (rel node)*.
func (p *parser) parsePattern() (*ast.Pattern, error) {
	start, err := p.parseNodePat()
	if err != nil {
		return nil, err
	}
	pat := &ast.Pattern{Start: *start}
	for p.peek().Kind == TokMinus || p.peek().Kind == TokLt {
		rel, rerr := p.parseRelPat()
		if rerr != nil {
			return nil, rerr
		}
		node, nerr := p.parseNodePat()
		if nerr != nil {
			return nil, nerr
		}
		pat.Hops = append(pat.Hops, ast.PatternHop{Rel: *rel, Node: *node})
	}
	return pat, nil
}

// parseNodePat is: '(' [var] [':' labelExpr] [propMap] ')'.
func (p *parser) parseNodePat() (*ast.NodePat, error) {
	if _, err := p.expect(TokLParen, "'(' starting a node pattern"); err != nil {
		return nil, err
	}
	n := &ast.NodePat{}
	if t := p.peek(); t.Kind == TokIdent {
		if reserved[strings.ToLower(t.Text)] {
			return nil, errf(t.Pos, "reserved word %q cannot be a node variable", t.Text)
		}
		n.Var = t.Text
		p.i++
	}
	if p.acceptTok(TokColon) {
		le, err := p.parseLabelOr()
		if err != nil {
			return nil, err
		}
		if labels, plain := flattenConjunction(le); plain {
			n.Labels = labels
		} else {
			n.LabelExpr = le
		}
	}
	if p.peek().Kind == TokLBrace {
		props, propExprs, err := p.parsePropMap()
		if err != nil {
			return nil, err
		}
		n.Props, n.PropExprs = props, propExprs
	}
	// Inline element predicate: (v:L WHERE expr) -- desugar conjoins it
	// onto the clause WHERE.
	if p.peekKw("where") {
		p.i++
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		n.Where = w
	}
	if _, err := p.expect(TokRParen, "')' closing a node pattern"); err != nil {
		return nil, err
	}
	return n, nil
}

// parseLabelOr is the label-expression grammar: or over and over not over
// (label | parens). Precedence: ! (tightest) > & (and ':') > |.
func (p *parser) parseLabelOr() (*ast.LabelExpr, error) {
	l, err := p.parseLabelAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptTok(TokPipe) {
		r, rerr := p.parseLabelAnd()
		if rerr != nil {
			return nil, rerr
		}
		l = &ast.LabelExpr{Kind: ast.LabelOr, L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseLabelAnd() (*ast.LabelExpr, error) {
	l, err := p.parseLabelNot()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TokAmp || p.peek().Kind == TokColon {
		p.i++
		r, rerr := p.parseLabelNot()
		if rerr != nil {
			return nil, rerr
		}
		l = &ast.LabelExpr{Kind: ast.LabelAnd, L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseLabelNot() (*ast.LabelExpr, error) {
	negs := 0
	for p.acceptTok(TokBang) {
		negs++
	}
	var le *ast.LabelExpr
	if p.acceptTok(TokPercent) {
		le = &ast.LabelExpr{Kind: ast.LabelWild}
		for range negs {
			le = &ast.LabelExpr{Kind: ast.LabelNot, L: le}
		}
		return le, nil
	}
	if p.acceptTok(TokLParen) {
		inner, err := p.parseLabelOr()
		if err != nil {
			return nil, err
		}
		if _, perr := p.expect(TokRParen, "')' closing a label expression"); perr != nil {
			return nil, perr
		}
		le = inner
	} else {
		name, err := p.identName("a label")
		if err != nil {
			return nil, err
		}
		le = &ast.LabelExpr{Kind: ast.LabelName, Name: name}
	}
	for range negs {
		le = &ast.LabelExpr{Kind: ast.LabelNot, L: le}
	}
	return le, nil
}

// flattenConjunction reports whether le is a plain conjunction of labels
// and returns them in written order (the planner fast path); a tree with
// any or/not stays a general label expression.
func flattenConjunction(le *ast.LabelExpr) ([]string, bool) {
	switch le.Kind {
	case ast.LabelName:
		return []string{le.Name}, true
	case ast.LabelAnd:
		l, lok := flattenConjunction(le.L)
		r, rok := flattenConjunction(le.R)
		if lok && rok {
			return append(l, r...), true
		}
	}
	return nil, false
}

// parsePropMap is: '{' key ':' expr [, ...] '}'. A literal value keeps the
// seek/filter fast path (Props); any other expression goes to PropExprs
// for the desugar pass to lower into a WHERE equality.
func (p *parser) parsePropMap() ([]ast.PropEntry, []ast.PropExprEntry, error) {
	p.i++ // '{'
	var props []ast.PropEntry
	var exprs []ast.PropExprEntry
	for p.peek().Kind != TokRBrace {
		key, err := p.identName("a property key")
		if err != nil {
			return nil, nil, err
		}
		if _, cerr := p.expect(TokColon, "':'"); cerr != nil {
			return nil, nil, cerr
		}
		e, eerr := p.parseExpr()
		if eerr != nil {
			return nil, nil, eerr
		}
		if lit, ok := e.(*ast.Lit); ok {
			props = append(props, ast.PropEntry{Key: key, Val: lit.Value})
		} else {
			exprs = append(exprs, ast.PropExprEntry{Key: key, Val: e})
		}
		if !p.acceptTok(TokComma) {
			break
		}
	}
	if _, err := p.expect(TokRBrace, "'}' closing a property map"); err != nil {
		return nil, nil, err
	}
	return props, exprs, nil
}

// parseRelPat is one relationship step: -[detail]->, <-[detail]-,
// -[detail]-, or the detail-free --, -->, <--; then an optional postfix
// quantifier.
func (p *parser) parseRelPat() (*ast.RelPat, error) {
	rel := &ast.RelPat{}
	leftArrow := false
	if p.acceptTok(TokLt) {
		leftArrow = true
	}
	if _, err := p.expect(TokMinus, "'-' in a relationship pattern"); err != nil {
		return nil, err
	}
	if p.peek().Kind == TokLBracket {
		if err := p.parseRelDetail(rel); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(TokMinus, "'-' closing a relationship pattern"); err != nil {
		return nil, err
	}
	rightArrow := false
	if !leftArrow && p.acceptTok(TokGt) {
		rightArrow = true
	}
	switch {
	case leftArrow:
		rel.Dir = ast.DirIn
	case rightArrow:
		rel.Dir = ast.DirOut
	default:
		rel.Dir = ast.DirBoth
	}
	if err := p.parseQuantifier(rel); err != nil {
		return nil, err
	}
	return rel, nil
}

// parseRelDetail is: '[' [var] [':' type ('|' type)*] [propMap] ']'. The
// Cypher in-bracket var-length (*1..3) is rejected in favor of the GQL
// postfix quantifier.
func (p *parser) parseRelDetail(rel *ast.RelPat) error {
	p.i++ // '['
	if t := p.peek(); t.Kind == TokIdent {
		if reserved[strings.ToLower(t.Text)] {
			return errf(t.Pos, "reserved word %q cannot be a relationship variable", t.Text)
		}
		rel.Var = t.Text
		p.i++
	}
	if p.acceptTok(TokColon) {
		for {
			name, err := p.identName("a relationship type")
			if err != nil {
				return err
			}
			rel.Types = append(rel.Types, name)
			if !p.acceptTok(TokPipe) {
				break
			}
		}
	}
	if p.peek().Kind == TokStar {
		return errf(p.peek().Pos, "in-bracket var-length (*m..n) is not GQL: quantify after the arrow, e.g. -[:T]->{1,3}")
	}
	if p.peek().Kind == TokLBrace {
		props, propExprs, err := p.parsePropMap()
		if err != nil {
			return err
		}
		rel.Props, rel.PropExprs = props, propExprs
	}
	// Inline element predicate: [r:T WHERE expr] -- desugar conjoins it
	// onto the clause WHERE.
	if p.peekKw("where") {
		p.i++
		w, err := p.parseExpr()
		if err != nil {
			return err
		}
		rel.Where = w
	}
	if _, err := p.expect(TokRBracket, "']' closing a relationship pattern"); err != nil {
		return err
	}
	return nil
}

// parseQuantifier is the optional GQL postfix quantifier after the arrow:
// {m,n} / {m,} / {,n} / {m} (exact) / * ({0,}) / + ({1,}).
func (p *parser) parseQuantifier(rel *ast.RelPat) error {
	switch p.peek().Kind {
	case TokQuestion:
		p.i++
		zero, one := uint64(0), uint64(1)
		rel.Length = &ast.VarLength{Min: &zero, Max: &one}
	case TokStar:
		p.i++
		rel.Length = &ast.VarLength{}
	case TokPlus:
		p.i++
		one := uint64(1)
		rel.Length = &ast.VarLength{Min: &one}
	case TokLBrace:
		// Only a quantifier follows an arrow with '{': digits and a comma.
		p.i++
		vl := &ast.VarLength{}
		if p.peek().Kind == TokInt {
			n, err := p.parseCount("quantifier")
			if err != nil {
				return err
			}
			vl.Min = &n
		}
		if p.acceptTok(TokComma) {
			if p.peek().Kind == TokInt {
				n, err := p.parseCount("quantifier")
				if err != nil {
					return err
				}
				vl.Max = &n
			}
		} else {
			if vl.Min == nil {
				return errf(p.peek().Pos, "empty quantifier: use {m,n}, {m,}, {,n}, or {m}")
			}
			vl.Max = vl.Min // {m} is exactly m
		}
		if _, err := p.expect(TokRBrace, "'}' closing a quantifier"); err != nil {
			return err
		}
		rel.Length = vl
	}
	return nil
}
