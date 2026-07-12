// Execute: run a plan's UNION branches and combine their rows (port of
// the Rust exec.rs entry points, minus the recognizer kernel dispatch).
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Execute runs a plan and returns its rows in output-column order. Every
// plan construct the planner produces is executable as of M19, so
// execution is infallible once a plan builds.
func Execute(ctx *eval.Ctx, p *plan.Plan) ([][]value.Value, error) {
	acc := runBranch(ctx, p.Branches[0])
	for i, op := range p.Union {
		combineUnion(&acc, runBranch(ctx, p.Branches[i+1]), op)
	}
	return acc, nil
}

// ExecuteProfiled runs the plan while recording how many rows each
// operator produces. Segment profiles are pushed branch-major,
// segment-minor -- the order the explain renderer walks. PROFILE reports
// operator cardinalities, not the result set; each branch's final rows
// are discarded (the union combine is not a profiled operator).
func ExecuteProfiled(ctx *eval.Ctx, p *plan.Plan) *explain.Profile {
	prof := &explain.Profile{}
	for _, segments := range p.Branches {
		rows := [][]value.Value{nil}
		for _, seg := range segments {
			sp := explain.SegProf{}
			rows = runSegment(ctx, seg, rows, &sp)
			prof.Segs = append(prof.Segs, sp)
		}
	}
	return prof
}

// runBranch runs one branch's segment pipeline from a single empty seed
// row, streaming across every run of boundaries whose upstream projection
// is per-row passthrough (rows cross a NEXT without materializing; the
// run's final segment materializes as before).
func runBranch(ctx *eval.Ctx, segments []*plan.Segment) [][]value.Value {
	rows := [][]value.Value{nil}
	for i := 0; i < len(segments); {
		// A columnar-aggregate chain fuses whole: the scan segment, its
		// LET boundaries, and the aggregated boundary run as one typed
		// pass; a declined chain falls through to the general runs.
		if segments[i].ColAgg {
			if out, n, ok := tryColumnarAggChain(ctx, segments, i, rows); ok {
				rows = out
				i += n
				continue
			}
		}
		j := i
		// Runs never stream INTO a chain head: the fused pass needs its
		// materialized seed rows.
		for j+1 < len(segments) && streamableBoundary(segments[j]) && !segments[j+1].ColAgg {
			j++
		}
		rows = runSegmentRun(ctx, segments[i:j+1], rows)
		i = j + 1
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
