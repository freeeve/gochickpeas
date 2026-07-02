// Package ast is the language-neutral query AST: produced by the GQL
// parser (gql/internal/parser), consumed by desugar/bind/plan. Port of the
// Rust cypher crate's ast.rs -- the shapes below the parser deliberately
// keep the Rust engine's segment model (a query is reading clauses plus
// projection boundaries ending in a final projection), so GQL's
// RETURN...NEXT / LET / FILTER forms all normalize into the same With
// clause the Rust binder and planner consume.
package ast

// QueryMode says how a query runs: normally, or as EXPLAIN (plan only) /
// PROFILE (plan annotated with actual per-operator row counts).
type QueryMode uint8

const (
	// Run executes normally.
	Run QueryMode = iota
	// Explain renders the plan without executing.
	Explain
	// Profile executes and annotates the plan with row counts.
	Profile
)

// UnionKind says how two adjacent QueryParts combine.
type UnionKind uint8

const (
	// UnionDistinct is UNION: combine branches, removing duplicate rows.
	UnionDistinct UnionKind = iota
	// UnionAll is UNION ALL: combine branches keeping every row.
	UnionAll
)

// Query is a whole read-only query: one or more QueryPart branches
// combined by UNION / UNION ALL. len(Union) == len(Parts)-1.
type Query struct {
	Mode  QueryMode
	Parts []QueryPart
	Union []UnionKind
}

// QueryPart is one UNION branch: zero or more reading / projection-boundary
// clauses, then the final projection.
type QueryPart struct {
	Clauses []Clause
	Ret     Projection
}

// Clause is a clause appearing before a part's final projection.
type Clause interface{ isClause() }

// Match is [OPTIONAL] MATCH pattern[, pattern...] [WHERE expr].
// Comma-separated patterns bind sequentially (sharing variables). When
// Optional, a row with no match survives with the new variables null.
type Match struct {
	Patterns []Pattern
	Where    Expr // nil when absent
	Optional bool
}

// With is the projection boundary: project columns forward, then
// optionally filter the projected rows. GQL surface forms that lower here:
// RETURN...NEXT (the projection), LET (star + extra items), FILTER (star +
// where).
type With struct {
	Proj  Projection
	Where Expr // nil when absent
}

// ShortestPath is a path-search binding: GQL `MATCH p = ANY SHORTEST pat`
// (All false) / `ALL SHORTEST pat` (All true), optionally weighted (no
// surface syntax yet; reachable for the engine). Binds PathVar between
// already-bound endpoints.
type ShortestPath struct {
	PathVar  string
	Pattern  Pattern
	Optional bool
	All      bool
	Weight   *CostSpec // nil for hop-minimal
	Where    Expr      // nil when absent
}

// CallProc is CALL proc(args...) YIELD field [AS alias], ... -- runs an
// analytic procedure and produces one row per result with the yielded
// columns bound.
type CallProc struct {
	Proc   string
	Args   []Literal
	Yields []YieldItem
}

// YieldItem is one YIELD field with an optional alias.
type YieldItem struct {
	Field string
	Alias string // "" when absent
}

// PathBind is [OPTIONAL] MATCH p = (a)-[...]->(b): bind the named path
// over a general pattern (distinct from the path-search form).
type PathBind struct {
	PathVar  string
	Pattern  Pattern
	Optional bool
	Where    Expr // nil when absent
}

// Unwind is GQL `FOR var IN expr`: expand a list to rows -- for each input
// row, emit one output row per element with var bound to it. An empty list
// or null emits no rows for that input row.
type Unwind struct {
	Expr Expr
	Var  string
}

// CallSubquery is CALL [(vars...)] { subquery }: a clause-level correlated
// subquery (lateral join). Imports is the variable-scope list -- the outer
// variables visible inside; empty means uncorrelated (runs once,
// cross-joins every outer row). The body query keeps a leading importing
// With as its first clause (the shape the binder expects).
type CallSubquery struct {
	Query   Query
	Imports []string
}

func (*Match) isClause()        {}
func (*With) isClause()         {}
func (*ShortestPath) isClause() {}
func (*CallProc) isClause()     {}
func (*PathBind) isClause()     {}
func (*Unwind) isClause()       {}
func (*CallSubquery) isClause() {}

// Projection is a projection body (With / final RETURN): items, DISTINCT,
// and the ordering / pagination applied to the projected rows.
type Projection struct {
	// Star means `*` was written: all in-scope variables project ahead of
	// any explicit Items, expanded by the binder in introduction order.
	Star     bool
	Distinct bool
	Items    []ReturnItem
	OrderBy  []SortItem
	Skip     *uint64 // OFFSET/SKIP; nil when absent
	Limit    *uint64 // nil when absent
}

// ReturnItem is one projected expression with an optional alias.
type ReturnItem struct {
	Expr  Expr
	Alias string // "" when absent
}

// SortItem is one ORDER BY key.
type SortItem struct {
	Expr Expr
	Desc bool
}

// Pattern is a linear path pattern: a start node followed by zero or more
// (rel, node) hops.
type Pattern struct {
	Start NodePat
	Hops  []PatternHop
}

// PatternHop is one relationship step and the node it reaches.
type PatternHop struct {
	Rel  RelPat
	Node NodePat
}

// EndNode is the node at the far end of the path (the start if no hops).
func (p *Pattern) EndNode() *NodePat {
	if len(p.Hops) == 0 {
		return &p.Start
	}
	return &p.Hops[len(p.Hops)-1].Node
}

// Reversed is the same path written end-to-start: the last node becomes
// the start and every relationship direction flips. Used by the planner to
// anchor traversal on the bound (selective) endpoint.
func (p *Pattern) Reversed() Pattern {
	nodes := make([]*NodePat, 0, len(p.Hops)+1)
	nodes = append(nodes, &p.Start)
	for i := range p.Hops {
		nodes = append(nodes, &p.Hops[i].Node)
	}
	out := Pattern{Start: *nodes[len(nodes)-1]}
	for i := len(p.Hops) - 1; i >= 0; i-- {
		rel := p.Hops[i].Rel
		rel.Dir = rel.Dir.Flipped()
		out.Hops = append(out.Hops, PatternHop{Rel: rel, Node: *nodes[i]})
	}
	return out
}

// NodePat is one node pattern.
type NodePat struct {
	Var string // "" when anonymous
	// Labels is the conjunctive (AND) label list -- the common case. When
	// the label expression is a plain conjunction it is flattened here and
	// LabelExpr stays nil, keeping the planner/kernel fast paths.
	Labels []string
	// LabelExpr is a general boolean label expression -- non-nil only when
	// not a plain conjunction (then Labels is empty). The planner lowers it
	// to a per-node HasLabelExpr WHERE conjunct.
	LabelExpr *LabelExpr
	// Props are literal/param inline property values ({id: 669},
	// {name: $p}) -- the fast seek/filter path.
	Props []PropEntry
	// PropExprs are non-literal inline property values ({name: tagVar}).
	// The desugar pass lowers each to a WHERE equality before planning.
	PropExprs []PropExprEntry
}

// PropEntry is one literal inline property.
type PropEntry struct {
	Key string
	Val Literal
}

// PropExprEntry is one expression-valued inline property.
type PropExprEntry struct {
	Key string
	Val Expr
}

// LabelKind discriminates a LabelExpr node.
type LabelKind uint8

const (
	// LabelName is a single label test.
	LabelName LabelKind = iota
	// LabelAnd is `a & b`.
	LabelAnd
	// LabelOr is `a | b`.
	LabelOr
	// LabelNot is `!a` (operand in L).
	LabelNot
)

// LabelExpr is a boolean label expression tree: label tests combined with
// & (and), | (or), ! (not), and parentheses.
type LabelExpr struct {
	Kind LabelKind
	Name string // LabelName only
	L, R *LabelExpr
}

// Dir is a relationship direction as written in the pattern.
type Dir uint8

const (
	// DirOut is -[...]->.
	DirOut Dir = iota
	// DirIn is <-[...]-.
	DirIn
	// DirBoth is -[...]-.
	DirBoth
)

// Flipped is the direction seen when traversing the other way.
func (d Dir) Flipped() Dir {
	switch d {
	case DirOut:
		return DirIn
	case DirIn:
		return DirOut
	}
	return DirBoth
}

// RelPat is one relationship pattern.
type RelPat struct {
	Var       string // "" when anonymous
	Dir       Dir
	Types     []string
	Props     []PropEntry
	PropExprs []PropExprEntry
	// Length is non-nil for a quantified relationship (-[:T]->{1,3}, *, +);
	// nil is a single fixed hop.
	Length *VarLength
}

// VarLength is a quantifier's bound spec. Nil bounds mean unspecified
// (`*` => both nil; `{2,}` => Max nil; `{,3}` => Min nil; `{2}` => Min ==
// Max).
type VarLength struct {
	Min *uint64
	Max *uint64
}

// CostKind discriminates a CostSpec.
type CostKind uint8

const (
	// CostProperty reads the named relationship property as the edge weight.
	CostProperty CostKind = iota
	// CostConstant gives every traversed relationship a fixed weight.
	CostConstant
	// CostExpr evaluates a per-edge weight formula referencing only the
	// path's relationship variable.
	CostExpr
)

// CostSpec is a weighted-path weight specification.
type CostSpec struct {
	Kind  CostKind
	Prop  string
	Const float64
	Expr  Expr
}
