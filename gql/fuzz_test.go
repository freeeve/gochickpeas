// FuzzEvalDiff: the compiled and interpreted eval paths must produce
// identical rows for any expression that parses and plans -- the Go
// replacement for the Rust rank-vs-cost differential fuzzer (the two eval
// paths are this port's result-identical A/B pair).
package gql

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

func FuzzEvalDiff(f *testing.F) {
	for _, seed := range []string{
		"p.age + 1",
		"p.age * 2 - p.joined / 10000",
		"-p.age",
		"p.name + '!'",
		"p.age IN [25, 30.0, null]",
		"p.name IN ['Alice', 'Bob']",
		"p.city IS NULL",
		"p.age > 30 AND p.city IS NOT NULL",
		"NOT (p.age > 30 OR p.name = 'Dave')",
		"CASE WHEN p.age > 30 THEN 'old' ELSE 'young' END",
		"CASE p.name WHEN 'Alice' THEN 1 WHEN 'Bob' THEN 2 END",
		"coalesce(p.city, 'nowhere')",
		"toString(p.age) + p.name",
		"substring(p.name, 1, 2)",
		"size(p.name) + size([1, 2])",
		"abs(-p.age)",
		"datetime({epochMillis: p.joined}).year",
		"date('2020-03-15') < p.joined",
		"p.name STARTS WITH 'A'",
		"[p.age, p.joined][0]",
		"[1, 2, 3, 4][1..3]",
		"all(y IN [1, p.age] WHERE y > 0)",
		"EXISTS { MATCH (p)-[:KNOWS]->(q) }",
		"COUNT { MATCH (p)-[:KNOWS]->(q) WHERE q.age > 30 }",
		"p:Person",
		"9223372036854775807 + p.age",
		"p.age / 0",
		"range(1, p.age / 10)",
		"{a: p.age, b: p.name}.a",
	} {
		f.Add(seed)
	}
	g := socialGraph(f)
	f.Fuzz(func(t *testing.T, expr string) {
		q := "MATCH (p:Person) RETURN " + expr + " AS x ORDER BY p.age"
		compiled, err := Run(g, q)
		if err != nil {
			// Parse/bind/plan errors happen before the paths split; both
			// paths reject identically by construction.
			return
		}
		forceInterp = true
		interp, ierr := Run(g, q)
		forceInterp = false
		if ierr != nil {
			t.Fatalf("interpreted path failed where compiled succeeded: %s\n%v", q, ierr)
		}
		for {
			cr, cok := compiled.Next()
			ir, iok := interp.Next()
			if cok != iok {
				t.Fatalf("row-count divergence: %s", q)
			}
			if !cok {
				return
			}
			cv, _ := cr.GetAt(0)
			iv, _ := ir.GetAt(0)
			if value.Key(cv) != value.Key(iv) {
				t.Fatalf("eval divergence on %q: compiled %v vs interpreted %v", expr, cv, iv)
			}
		}
	})
}
