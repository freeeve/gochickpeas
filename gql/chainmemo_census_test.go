// Chain-memo admission census (task 195, rust 17ecfe0's arc): before
// building any suffix-completion memo, measure WHERE it could legally
// fire. A stage qualifies for the single-slot variant when its ops read
// exactly ONE externally-bound slot (the memo key) and every
// relationship-uniqueness-tracked op is replay-safe under the hash-join
// pair discipline: tracked FIXED expands replay (pairs re-checked
// against the live env per row), tracked VAR-EXPANDS decline -- a
// checked walk's own pruning depends on the row's live pairs, which a
// keyed build cannot see. Detection only, consumed by nothing; the
// verdict decides whether the exec mechanism is worth building.
//
// Gated on GOCHICKPEAS_GQL_MANIFEST; run under the local-cpu lock.
package gql

import (
	"fmt"
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// stageMemoVerdict classifies one MatchStage of a built plan.
func stageMemoVerdict(ms *plan.MatchStage, slots map[string]int) (keyed bool, reason string) {
	written := map[int]bool{}
	external := map[int]bool{}
	readSlot := func(s int) {
		if s >= 0 && !written[s] {
			external[s] = true
		}
	}
	trackedVarExpand := false
	for i := range ms.Ops {
		op := &ms.Ops[i]
		switch op.Kind {
		case plan.OpScan:
			if op.Source.Kind == plan.ScanArg {
				readSlot(op.Slot)
			} else {
				written[op.Slot] = true
			}
			if op.Source.Slot != plan.NoSlot && op.Source.Kind == plan.ScanArg {
				readSlot(op.Source.Slot)
			}
		case plan.OpExpand, plan.OpVarExpand:
			readSlot(op.From)
			if op.Rebind {
				readSlot(op.To)
			} else {
				written[op.To] = true
			}
			if op.RelSlot != plan.NoSlot {
				written[op.RelSlot] = true
			}
			if op.Kind == plan.OpVarExpand && op.Uniq != nil && op.Uniq.Check {
				trackedVarExpand = true
			}
		}
	}
	if ms.Where != nil {
		ast.Walk(ms.Where, func(e ast.Expr) bool {
			switch n := e.(type) {
			case *ast.Var:
				if s, ok := slots[n.Name]; ok {
					readSlot(s)
				}
			case *ast.Prop:
				if s, ok := slots[n.Var]; ok {
					readSlot(s)
				}
			}
			return true
		})
	}
	switch {
	case len(external) != 1:
		return false, fmt.Sprintf("reads %d external slots", len(external))
	case trackedVarExpand:
		return false, "check-tracked var-expand (walk pruning depends on live pairs)"
	default:
		return true, ""
	}
}

func TestChainMemoAdmissionCensus(t *testing.T) {
	manifest := os.Getenv("GOCHICKPEAS_GQL_MANIFEST")
	if manifest == "" {
		t.Skip("set GOCHICKPEAS_GQL_MANIFEST to run the census")
	}
	rows, err := ldbc.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	graphs := map[string]*chickpeas.Snapshot{}
	qualifying := 0
	for _, row := range rows {
		if row.Blocked() {
			continue
		}
		g, ok := graphs[row.Graph]
		if !ok {
			g, err = chickpeas.ReadRCPGFile(row.Graph)
			if err != nil {
				t.Fatalf("loading %s: %v", row.Graph, err)
			}
			graphs[row.Graph] = g
		}
		qs, err := parseDesugar(row.GQL)
		if err != nil {
			t.Fatalf("%s: %v", row.Query, err)
		}
		p, err := plan.Build(qs, graph.New(g))
		if err != nil {
			t.Fatalf("%s: %v", row.Query, err)
		}
		for _, segs := range p.Branches {
			for si, seg := range segs {
				// Only NON-FIRST stages can key on a prior slot; the memo
				// pays off on the trailing stages a wide intermediate
				// re-executes per row.
				for sti, st := range seg.Stages {
					ms, ok := st.(*plan.MatchStage)
					if !ok || sti == 0 {
						continue
					}
					if keyed, reason := stageMemoVerdict(ms, seg.Slots); keyed {
						fmt.Printf("memocensus QUALIFIES %-16s seg=%d stage=%d ops=%d\n",
							row.Family+"/"+row.Query, si, sti, len(ms.Ops))
						qualifying++
					} else if reason != "reads 0 external slots" {
						_ = reason
					}
				}
			}
		}
	}
	fmt.Printf("memocensus total qualifying stages: %d\n", qualifying)
}
