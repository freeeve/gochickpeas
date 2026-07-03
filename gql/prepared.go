// Prepared: a parsed, auto-parameterized, and planned query, ready to
// execute repeatedly without re-incurring parse/plan cost -- the unit the
// plan cache holds, exposed directly for callers managing their own reuse.
package gql

import (
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Prepared is a compiled query. The plan is auto-parameterized (its
// inline constants are lifted and stored), so it is value-independent by
// construction; its cost-based choices reflect the statistics of the
// snapshot it was prepared against -- executing against another snapshot
// is legal and correct, possibly suboptimal. An EXPLAIN/PROFILE query
// prepares too: Execute then returns the rendered plan.
type Prepared struct {
	plan     *plan.Plan
	mode     ast.QueryMode
	lifted   []value.Value
	planTime time.Duration
}

// Prepare parses, desugars, auto-parameterizes, and plans query once
// against g's statistics.
func Prepare(g *chickpeas.Snapshot, query string) (*Prepared, error) {
	start := time.Now()
	q, err := parseDesugar(query)
	if err != nil {
		return nil, err
	}
	lifted := semantics.AutoParameterize(q)
	p, err := plan.Build(q, graph.New(g))
	if err != nil {
		return nil, wrapStage(err)
	}
	return &Prepared{plan: p, mode: q.Mode, lifted: lifted, planTime: time.Since(start)}, nil
}

// Execute runs the prepared plan against g with explicit $name parameter
// values (an unsupplied parameter reads as null); the stored lifted
// constants rebind automatically. For an EXPLAIN/PROFILE query this
// returns the rendered plan instead.
func (pr *Prepared) Execute(g *chickpeas.Snapshot, params map[string]value.Value) (*Rows, error) {
	gr := graph.New(g)
	ctx := &eval.Ctx{G: gr, Params: pr.lifted, Named: params, ForceInterp: forceInterp}
	return execPlan(gr, pr.plan, pr.mode, pr.planTime, ctx)
}

// Columns is the output column names the plan produces, in order.
func (pr *Prepared) Columns() []string { return pr.plan.Columns }
