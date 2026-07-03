// Sub-plan execution for CALL { } subqueries (the per-row lateral-join
// sinks live in stream.go).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// runSubplan runs a sub-plan over one seed row, combining its UNION
// branches exactly like the top-level Execute.
func runSubplan(ctx *eval.Ctx, sub *plan.Plan, seed []value.Value) [][]value.Value {
	acc := runBranchSeeded(ctx, sub.Branches[0], seed)
	for i, op := range sub.Union {
		combineUnion(&acc, runBranchSeeded(ctx, sub.Branches[i+1], seed), op)
	}
	return acc
}

// runBranchSeeded runs a branch's segment pipeline from one seed row.
// Sub-plans are never profiled (a CALL {} stage records a single output
// count, matching the Rust engine).
func runBranchSeeded(ctx *eval.Ctx, segments []*plan.Segment, seed []value.Value) [][]value.Value {
	rows := [][]value.Value{seed}
	for _, seg := range segments {
		rows = runSegment(ctx, seg, rows, nil)
	}
	return rows
}
