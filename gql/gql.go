// The public entry points: Run/RunWithParams execute a GQL query over an
// engine snapshot; Explain renders the plan. Pipeline: parse -> desugar ->
// plan (cost-based) -> execute, mapping each stage's typed error onto the
// package sentinels.
package gql

import (
	"errors"
	"fmt"
	"strings"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/exec"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Run executes a GQL query over the snapshot.
func Run(g *chickpeas.Snapshot, query string) (*Rows, error) {
	return RunWithParams(g, query, nil)
}

// RunWithParams executes a GQL query with explicit $name parameter values.
// An unsupplied parameter reads as null.
func RunWithParams(g *chickpeas.Snapshot, query string, params map[string]value.Value) (*Rows, error) {
	q, gr, p, planTime, err := prepare(g, query)
	if err != nil {
		return nil, err
	}
	ctx := &eval.Ctx{G: gr, Named: params, ForceInterp: forceInterp}
	return execPlan(gr, p, q.Mode, planTime, ctx)
}

// execPlan executes a built plan in its query mode: Run returns result
// rows; EXPLAIN returns the rendered plan; PROFILE executes while
// recording per-operator produced-row counts and returns the annotated
// plan (cardinalities, not the result set).
func execPlan(gr *graph.SnapshotGraph, p *plan.Plan, mode ast.QueryMode, planTime time.Duration, ctx *eval.Ctx) (*Rows, error) {
	switch mode {
	case ast.Explain:
		return renderPlanRows(gr, p, nil, planTime), nil
	case ast.Profile:
		return renderPlanRows(gr, p, exec.ExecuteProfiled(ctx, p), planTime), nil
	}
	rows, err := exec.Execute(ctx, p)
	if err != nil {
		return nil, wrapStage(err)
	}
	return newRows(p.Columns, rows), nil
}

// Explain renders the query's plan (with cardinality estimates and anchor
// notes) without executing it.
func Explain(g *chickpeas.Snapshot, query string) (string, error) {
	_, gr, p, planTime, err := prepare(g, query)
	if err != nil {
		return "", err
	}
	return strings.Join(explain.Render(p, nil, planTime, plan.Estimate(p, gr)), "\n"), nil
}

// ExplainCanonical renders the query's plan as a stable, canonical shape string
// for golden plan-regression snapshots: operator order, scan sources, hop
// patterns, the recognizers that fired, and the chosen anchor -- with the
// volatile parts (wall-clock planning time, the numeric estimates, the anchor
// note's magnitudes) excluded. Unlike Explain the output is deterministic across
// plannings, so a diff between two commits is a review prompt that a planner
// change moved the plan, invisible to a row-level differential.
func ExplainCanonical(g *chickpeas.Snapshot, query string) (string, error) {
	_, gr, p, _, err := prepare(g, query)
	if err != nil {
		return "", err
	}
	return strings.Join(explain.Canonical(p, plan.Estimate(p, gr)), "\n"), nil
}

// prepare runs the shared front half: parse, desugar, plan -- timing the
// planning for the EXPLAIN/PROFILE header.
func prepare(g *chickpeas.Snapshot, query string) (*ast.Query, *graph.SnapshotGraph, *plan.Plan, time.Duration, error) {
	start := time.Now()
	q, err := parseDesugar(query)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	gr := graph.New(g)
	p, err := plan.Build(q, gr)
	if err != nil {
		return nil, nil, nil, 0, wrapStage(err)
	}
	return q, gr, p, time.Since(start), nil
}

// parseDesugar is the language front half: parse, then normalize.
func parseDesugar(query string) (*ast.Query, error) {
	q, err := parser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", err.Error(), ErrParse)
	}
	if err := semantics.Desugar(q); err != nil {
		return nil, wrapStage(err)
	}
	return q, nil
}

// renderPlanRows renders an EXPLAIN/PROFILE-mode query as a one-column
// result (per-operator counts included when prof is non-nil).
func renderPlanRows(gr graph.Graph, p *plan.Plan, prof *explain.Profile, planTime time.Duration) *Rows {
	lines := explain.Render(p, prof, planTime, plan.Estimate(p, gr))
	rows := make([][]value.Value, len(lines))
	for i, l := range lines {
		rows[i] = []value.Value{value.Str(l)}
	}
	return newRows([]string{"plan"}, rows)
}

// forceInterp pins execution to the interpreted eval path -- the
// dual-path differential test hook (package tests set it; never exported).
var forceInterp bool

// wrapStage maps a pipeline stage's typed error onto the package
// sentinels, keeping the message.
func wrapStage(err error) error {
	var se *semantics.Error
	if errors.As(err, &se) {
		switch se.Kind {
		case semantics.KindBind:
			return fmt.Errorf("%s: %w", se.Msg, ErrBind)
		default:
			return fmt.Errorf("%s: %w", se.Msg, ErrPlan)
		}
	}
	return err
}
