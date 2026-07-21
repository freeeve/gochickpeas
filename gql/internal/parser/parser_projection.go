// Projection parsing: the [DISTINCT] item-list plus the shared
// ORDER BY / OFFSET|SKIP / LIMIT suffix (used by RETURN and the standalone
// ORDER BY statement). Split from parser.go, which holds the query and
// clause parsing.
package parser

import (
	"strconv"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

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
