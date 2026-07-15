// Benchmark for the value-keyed hash join (task 108): two disconnected
// components joined only by a property equality. The A side flips the
// join on/off via the exported thresholds in one process, so both arms
// run the same binary on the same graph.
package gql

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

func valueJoinBenchGraph(b *testing.B, n int) *chickpeas.Snapshot {
	b.Helper()
	bl := chickpeas.NewBuilder(2*n+2, 2)
	for i := 0; i < n; i++ {
		p, err := bl.AddNode("Person")
		if err != nil {
			b.Fatal(err)
		}
		_ = bl.SetProp(p, "email", fmt.Sprintf("u%d@x", i))
	}
	for i := 0; i < n; i++ {
		a, err := bl.AddNode("Account")
		if err != nil {
			b.Fatal(err)
		}
		// Half the accounts match a person's email.
		if i%2 == 0 {
			_ = bl.SetProp(a, "email", fmt.Sprintf("u%d@x", i))
		} else {
			_ = bl.SetProp(a, "email", fmt.Sprintf("nobody%d@y", i))
		}
	}
	return bl.Finalize("email")
}

func benchValueJoin(b *testing.B, join bool) {
	g := valueJoinBenchGraph(b, 2000)
	q := "MATCH (p:Person), (a:Account) WHERE p.email = a.email RETURN count(*) AS n"
	mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
	if join {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 0, 2, 4
	} else {
		plan.HashJoinMinRows = 1 << 62 // never
	}
	defer func() {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed
	}()
	b.ResetTimer()
	for b.Loop() {
		rows, err := Run(g, q)
		if err != nil {
			b.Fatal(err)
		}
		r, _ := rows.Next()
		if v, _ := r.GetAt(0); func() int64 { i, _ := v.AsInt(); return i }() != 1000 {
			b.Fatalf("count = %v, want 1000", v)
		}
	}
}

func BenchmarkValueJoinNested(b *testing.B) { benchValueJoin(b, false) }
func BenchmarkValueJoinHash(b *testing.B)   { benchValueJoin(b, true) }
