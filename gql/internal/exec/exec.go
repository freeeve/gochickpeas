// Execute: run a plan's UNION branches and combine their rows (port of
// the Rust exec.rs entry points, minus the recognizer kernel dispatch).
package exec

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Execute runs a plan and returns its rows in output-column order.
func Execute(ctx *eval.Ctx, p *plan.Plan) ([][]value.Value, error) {
	if err := checkSupported(p); err != nil {
		return nil, err
	}
	acc := runBranch(ctx, p.Branches[0])
	for i, op := range p.Union {
		combineUnion(&acc, runBranch(ctx, p.Branches[i+1]), op)
	}
	return acc, nil
}

// runBranch runs one branch's segment pipeline from a single empty seed
// row.
func runBranch(ctx *eval.Ctx, segments []*plan.Segment) [][]value.Value {
	rows := [][]value.Value{nil}
	for _, seg := range segments {
		rows = runSegment(ctx, seg, rows)
	}
	return rows
}

// combineUnion folds a later branch's rows into the accumulator. UNION ALL
// concatenates; UNION concatenates then dedups the whole accumulated set
// keeping first-occurrence order (left-associative). The dedup key matches
// DISTINCT's group key, so UNION and RETURN DISTINCT agree on row
// identity.
func combineUnion(acc *[][]value.Value, next [][]value.Value, op ast.UnionKind) {
	*acc = append(*acc, next...)
	if op != ast.UnionDistinct {
		return
	}
	seen := make(map[string]struct{}, len(*acc))
	kept := (*acc)[:0]
	var key []byte
	for _, r := range *acc {
		key = key[:0]
		for _, v := range r {
			key = value.AppendKey(key, v)
		}
		if _, dup := seen[string(key)]; !dup {
			seen[string(key)] = struct{}{}
			kept = append(kept, r)
		}
	}
	*acc = kept
}

// checkSupported rejects plan constructs whose executors land in later
// milestones, so the error is a typed plan error instead of a wrong
// result. TODO(M17/M18/M19): remove arms as the executors land.
func checkSupported(p *plan.Plan) error {
	for _, branch := range p.Branches {
		for _, seg := range branch {
			for _, st := range seg.Stages {
				switch s := st.(type) {
				case *plan.MatchStage:
					// Fully supported since M17.
				case *plan.SpStage:
					if s.Weight != nil {
						return unsupported("weighted shortest paths (M19)")
					}
				case *plan.CallStage:
					return unsupported("CALL procedures (M19)")
				case *plan.CallSubqueryStage:
					// The sub-plan may itself carry gated stages.
					if err := checkSupported(s.Sub); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// unsupported is a KindPlan error for a not-yet-ported executor feature.
func unsupported(what string) error {
	return &semantics.Error{Kind: semantics.KindPlan, Msg: fmt.Sprintf("not yet supported: %s", what)}
}
