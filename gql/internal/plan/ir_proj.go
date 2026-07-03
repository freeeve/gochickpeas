// Projection-side IR: the compiled projection (ProjPlan), segments, the
// whole Plan, the pre-segment stage specs, and the plan-error helper.
package plan

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// BoundReturn is one projected column.
type BoundReturn struct {
	Expr  ast.Expr
	Name  string
	IsAgg bool
}

// PostProj is one post-aggregation scalar projection: output column Col is
// computed by Expr (rewritten to read hidden __agg{k} slots) after the
// groups finalize.
type PostProj struct {
	Col  int
	Expr ast.Expr
}

// ProjPlan is a compiled projection (a With boundary or the final RETURN).
type ProjPlan struct {
	Returns    []BoundReturn
	Distinct   bool
	Aggregated bool
	// GroupIdx are the non-aggregate output columns (the group key).
	GroupIdx []int
	Aggs     []AggCol
	// Post are the nested-aggregate scalar wrappers; NHidden counts the
	// hidden accumulator slots feeding them (per-group rows are sized
	// len(Returns)+NHidden, truncated back after Post runs).
	Post    []PostProj
	NHidden int
	OrderBy []ast.SortItem
	Skip    *uint64
	Limit   *uint64
	Columns []string
}

// Segment is one pipeline stage run: the stages in order, then the
// projection. Output columns feed the next segment.
type Segment struct {
	Stages   []Stage
	RowWidth int
	Slots    map[string]int
	Proj     ProjPlan
	// PostWhere filters projected rows (references output columns) -- the
	// projection boundary's WHERE.
	PostWhere ast.Expr
}

// Plan is a compiled query: one segment pipeline per UNION branch, the
// combinators between them, and the output column names (identical across
// branches, validated at plan time).
type Plan struct {
	Branches [][]*Segment
	Union    []ast.UnionKind
	Columns  []string
}

// specKind discriminates a stageSpec.
type specKind uint8

const (
	specMatch specKind = iota
	specShortest
	specCall
	specUnwind
	specCallSubquery
)

// stageSpec is one clause accumulated before its segment is built (at the
// next projection boundary).
type stageSpec struct {
	kind specKind

	// specMatch / specShortest
	pattern  *ast.Pattern
	where    ast.Expr
	optional bool
	pathVar  string // named-path bind ("" none)

	// specShortest
	all    bool
	weight *ast.CostSpec

	// specCall
	proc   string
	args   []ast.Literal
	yields []ast.YieldItem

	// specUnwind
	list    ast.Expr
	varName string

	// specCallSubquery
	query   *ast.Query
	imports []string
}

// planErrf is a plan-stage error (wrapped onto ErrPlan by the root package).
func planErrf(format string, args ...any) error {
	return &semantics.Error{Kind: semantics.KindPlan, Msg: fmt.Sprintf(format, args...)}
}

// bindErrf is a bind-stage error surfaced during planning (unknown
// variable, UNION column mismatch, aggregate misuse).
func bindErrf(format string, args ...any) error {
	return &semantics.Error{Kind: semantics.KindBind, Msg: fmt.Sprintf(format, args...)}
}
