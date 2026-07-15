// Allocation A/B harness for the BI Q1 full-scan aggregation (task 147). BI
// Q1 scans every Message on SF1 twice (a count, then a grouped aggregate over
// year/isComment/lengthCategory), so its per-row allocation cost is the
// cleanest magnifier of the gql pipeline's scan->filter->project->aggregate
// allocation profile -- the card that opened 147 put it at ~875 allocs /
// 128.8 MB versus rust-native's zero. Env-gated on GOCHICKPEAS_SF1_RCPG like
// the other SF1 validations; drive with:
//
//	GOCHICKPEAS_SF1_RCPG=~/rustychickpeas-ldbc/export/sf1_canonical.rcpg \
//	  go test ./gql -run '^$' -bench BenchmarkBIQ1Allocs -benchmem \
//	  -memprofile /tmp/biq1.mem && go tool pprof -alloc_space /tmp/biq1.mem
package gql

import (
	"os"
	"runtime"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/exec"
)

// biQ1 is the BI Q1 canonical GQL from the parity manifest: a two-phase
// message aggregation with a CASE length bucket and a label predicate.
const biQ1 = `MATCH (message:Message) WHERE message.creationDate < zoned_datetime('2011-12-01') RETURN count(message) AS totalMessageCountInt ` +
	`NEXT LET totalMessageCount = CAST(totalMessageCountInt AS FLOAT) ` +
	`NEXT MATCH (message:Message) WHERE message.creationDate < zoned_datetime('2011-12-01') AND message.content IS NOT NULL ` +
	`LET year = message.creationDate.year LET isComment = message IS LABELED Comment ` +
	`LET lengthCategory = CASE WHEN message.length < 40 THEN 0 WHEN message.length < 80 THEN 1 WHEN message.length < 160 THEN 2 ELSE 3 END ` +
	`RETURN totalMessageCount, year, isComment, lengthCategory, count(message) AS messageCount, ` +
	`sum(message.length) / CAST(count(message) AS FLOAT) AS averageMessageLength, sum(message.length) AS sumMessageLength ` +
	`NEXT RETURN year, isComment, lengthCategory, messageCount, averageMessageLength, sumMessageLength, ` +
	`messageCount / totalMessageCount AS percentageOfMessages ORDER BY year DESC, isComment ASC, lengthCategory ASC`

func loadSF1Bench(b *testing.B) *chickpeas.Snapshot {
	b.Helper()
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		b.Skip("GOCHICKPEAS_SF1_RCPG unset; skipping SF1 alloc benchmark")
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		b.Fatalf("loading %s: %v", path, err)
	}
	return g
}

func BenchmarkBIQ1Allocs(b *testing.B) {
	g := loadSF1Bench(b)
	// With GOCHICKPEAS_MEMRATE1 set, profile every allocation (set AFTER the
	// load so the one-time graph load stays sparsely sampled and the query
	// region is full fidelity) -- the way to get an exact per-site count of
	// the ~700 query-path allocs from -memprofile.
	if os.Getenv("GOCHICKPEAS_MEMRATE1") != "" {
		old := runtime.MemProfileRate
		runtime.MemProfileRate = 1
		defer func() { runtime.MemProfileRate = old }()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := Run(g, biQ1)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		n := 0
		for {
			if _, ok := rows.Next(); !ok {
				break
			}
			n++
		}
		if n == 0 {
			b.Fatal("no rows")
		}
	}
}

// BenchmarkBIQ1ExecAllocs isolates the EXECUTION-path allocations: the plan
// is parsed and built once (setup), then executed per iteration -- the cost a
// served / plan-cached workload actually pays each run, and the fair
// comparison to a native engine that builds the plan once. A fresh Ctx per
// run keeps prepared-plan reuse safe (the Ctx holds the per-execution
// subquery-shape cache).
func BenchmarkBIQ1ExecAllocs(b *testing.B) {
	g := loadSF1Bench(b)
	_, gr, p, _, err := prepare(g, biQ1)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	if os.Getenv("GOCHICKPEAS_MEMRATE1") != "" {
		old := runtime.MemProfileRate
		runtime.MemProfileRate = 1
		defer func() { runtime.MemProfileRate = old }()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := exec.Execute(&eval.Ctx{G: gr}, p)
		if err != nil {
			b.Fatalf("execute: %v", err)
		}
		if len(rows) == 0 {
			b.Fatal("no rows")
		}
	}
}
