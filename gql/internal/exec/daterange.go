// Date-truncation range rewrite: a WHERE conjunct of the shape
// date(<var>.<key>) = <constant Date> tests day membership, which over an
// epoch-millis integer column is exactly the half-open range
// [day, day + 24h) on the RAW value. Rewriting the conjunct to two
// property-vs-constant comparisons lets the pushdown machinery place and
// batch-sweep them like any other integer filter, instead of evaluating a
// temporal constructor per row -- the dominant filter shape in date-heavy
// analytic queries. The rewrite fires on generic structure (a resolved
// date() call over an integer-or-absent column against a constant Date),
// never on query identity.
package exec

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/compile"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// dateRangeRewrites counts fired rewrites -- the engagement oracle: a
// date(prop) = const conjunct over an integer column must rewrite exactly
// once (two range conjuncts replace it); 0 on a qualifying shape means
// the rewrite is dead.
var dateRangeRewrites int

// disableDateRangeRewrite pins result identity in tests: the general
// per-row evaluation must produce exactly the rows the rewrite does.
var disableDateRangeRewrite bool

// rewriteDateEqRanges maps each qualifying conjunct to its two range
// conjuncts, leaving everything else untouched.
func rewriteDateEqRanges(ctx *eval.Ctx, conjs []ast.Expr, slots map[string]int) []ast.Expr {
	if disableDateRangeRewrite {
		return conjs
	}
	n, isNative := ctx.G.(graph.Native)
	if !isNative {
		return conjs
	}
	g := n.Snapshot()
	out := make([]ast.Expr, 0, len(conjs))
	for _, c := range conjs {
		if lo, hi, ok := dateEqRange(ctx, g, c, slots); ok {
			out = append(out, lo, hi)
			continue
		}
		out = append(out, c)
	}
	return out
}

// dateEqRange matches date(<prop>) = <const Date> (either operand order)
// and returns the equivalent raw-value range conjuncts. The property's
// resolvable columns must read integer-or-absent -- an integer value
// floors to the day exactly as the range tests it, an absent value nulls
// out of both forms identically, and any other column type keeps the
// general evaluation.
func dateEqRange(ctx *eval.Ctx, g *chickpeas.Snapshot, e ast.Expr, slots map[string]int) (lo, hi ast.Expr, ok bool) {
	bin, isBin := e.(*ast.Binary)
	if !isBin || bin.Op != ast.OpEq {
		return nil, nil, false
	}
	prop, constSide, found := datePropAndConst(bin.LHS, bin.RHS)
	if !found {
		prop, constSide, found = datePropAndConst(bin.RHS, bin.LHS)
	}
	if !found || !i64OrAbsentColumns(g, prop.Key) {
		return nil, nil, false
	}
	cv, isConst := compile.ConstValue(ctx, constSide, slots, g)
	if !isConst {
		return nil, nil, false
	}
	ms, kind, isTemporal := cv.AsTemporal()
	if !isTemporal || kind != value.Date {
		return nil, nil, false
	}
	dateRangeRewrites++
	return &ast.Binary{Op: ast.OpGte, LHS: prop, RHS: &ast.Lit{Value: ast.IntLit(ms)}},
		&ast.Binary{Op: ast.OpLt, LHS: prop, RHS: &ast.Lit{Value: ast.IntLit(ms + eval.MSPerDay)}},
		true
}

// datePropAndConst matches one operand orientation: a resolved date()
// call over a bare property read, paired with the other side as the
// constant candidate.
func datePropAndConst(fnSide, other ast.Expr) (*ast.Prop, ast.Expr, bool) {
	f, isFunc := fnSide.(*ast.Func)
	if !isFunc || f.Distinct || f.Star || len(f.Args) != 1 {
		return nil, nil, false
	}
	if op, resolved := eval.ResolveFuncOp(f.Name); !resolved || op != eval.FuncDate {
		return nil, nil, false
	}
	p, isProp := f.Args[0].(*ast.Prop)
	if !isProp {
		return nil, nil, false
	}
	return p, other, true
}

// i64OrAbsentColumns reports whether every column that could serve a read
// of key yields an integer or absent: a missing column reads absent for
// every position, and an integer column is exactly the epoch-millis shape
// date() floors. A float, boolean, or string column declines the rewrite.
func i64OrAbsentColumns(g *chickpeas.Snapshot, key string) bool {
	if c, ok := g.ColIndexed(key); ok && c.Dtype() != chickpeas.DtypeI64 {
		return false
	}
	if c, ok := g.RelColIndexed(key); ok && c.Dtype() != chickpeas.DtypeI64 {
		return false
	}
	return true
}
