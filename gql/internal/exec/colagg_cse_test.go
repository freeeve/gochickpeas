// Cross-segment scan CSE at execution: the recorder's fused pass
// publishes survivors, the consumer seeds from them, and every fallback
// leg (disabled marking, failed param condition, recorder declined to
// the general path) produces identical rows. Comparisons are in engine
// order -- the fixtures carry ORDER BY over unique keys, so sequence is
// contractual -- and every shared run asserts the engagement counters,
// so no leg can pass vacuously on an unshared pipeline.
package exec

import (
	"fmt"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// cseRun plans and executes q, optionally auto-parameterized, returning
// rendered rows in engine order.
func cseRun(t *testing.T, g *chickpeas.Snapshot, q string, template bool) []string {
	t.Helper()
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lifted []value.Value
	if template {
		lifted = semantics.AutoParameterize(qq)
	}
	p, err := plan.Build(qq, graph.New(g))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ctx := &eval.Ctx{G: graph.New(g), Params: lifted}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r))
	}
	return out
}

const cseChainQ = `MATCH (m:Message) WHERE m.length < 150 RETURN count(m) AS total
 NEXT MATCH (m:Message) WHERE m.length < 150 AND m.length > 20
 RETURN total, m.length AS l, count(m) AS n
 NEXT RETURN total, l, n ORDER BY l`

func TestScanCSESharesAndMatchesUnshared(t *testing.T) {
	g := colAggFixture(t)
	rec0, cons0 := colAggCSERecorded, colAggCSEConsumed
	shared := cseRun(t, g, cseChainQ, false)
	if colAggCSERecorded != rec0+1 || colAggCSEConsumed != cons0+1 {
		t.Fatalf("counters after shared run: recorded +%d, consumed +%d, want +1/+1",
			colAggCSERecorded-rec0, colAggCSEConsumed-cons0)
	}
	plan.DisableScanCSE = true
	defer func() { plan.DisableScanCSE = false }()
	unshared := cseRun(t, g, cseChainQ, false)
	if !slices.Equal(shared, unshared) {
		t.Fatalf("ordered-row divergence:\nshared   (%d): %v\nunshared (%d): %v",
			len(shared), shared, len(unshared), unshared)
	}
	if len(shared) == 0 {
		t.Fatal("0 rows -- the differential measured nothing")
	}
}

// An identical consumer filter shares with an empty residual: the memo
// ids feed the aggregate with no conjuncts at all.
func TestScanCSEEmptyResidual(t *testing.T) {
	g := colAggFixture(t)
	q := `MATCH (m:Message) WHERE m.length < 150 RETURN count(m) AS total
 NEXT MATCH (m:Message) WHERE m.length < 150
 RETURN total, m.length AS l, count(m) AS n
 NEXT RETURN total, l, n ORDER BY l`
	cons0 := colAggCSEConsumed
	shared := cseRun(t, g, q, false)
	if colAggCSEConsumed != cons0+1 {
		t.Fatal("consumer did not engage on the identical-filter pair")
	}
	plan.DisableScanCSE = true
	defer func() { plan.DisableScanCSE = false }()
	if unshared := cseRun(t, g, q, false); !slices.Equal(shared, unshared) {
		t.Fatalf("ordered-row divergence:\nshared: %v\nunshared: %v", shared, unshared)
	}
}

// A cached template whose lifted bounds differ must NOT share: the two
// conjuncts are one shape with unequal slot values, the param condition
// fails at execution, and the consumer runs its full filter.
func TestScanCSEParamCondBlocksUnequalBounds(t *testing.T) {
	g := colAggFixture(t)
	q := `MATCH (m:Message) WHERE m.length < 150 RETURN count(m) AS total
 NEXT MATCH (m:Message) WHERE m.length < 90 AND m.length > 20
 RETURN total, m.length AS l, count(m) AS n
 NEXT RETURN total, l, n ORDER BY l`
	cons0 := colAggCSEConsumed
	templated := cseRun(t, g, q, true)
	if colAggCSEConsumed != cons0 {
		t.Fatal("consumer engaged despite unequal bound values -- the param condition failed open")
	}
	if literal := cseRun(t, g, q, false); !slices.Equal(templated, literal) {
		t.Fatalf("template/literal divergence:\ntemplate: %v\nliteral:  %v", templated, literal)
	}
	if len(templated) == 0 {
		t.Fatal("0 rows -- the fallback measured nothing")
	}
}

// Equal lifted bounds through the template path DO share -- the
// condition holds, so the cached form gets the same win as the literal
// form.
func TestScanCSEParamCondHoldsEqualBounds(t *testing.T) {
	g := colAggFixture(t)
	cons0 := colAggCSEConsumed
	templated := cseRun(t, g, cseChainQ, true)
	if colAggCSEConsumed != cons0+1 {
		t.Fatal("consumer did not engage on the equal-bound template")
	}
	if literal := cseRun(t, g, cseChainQ, false); !slices.Equal(templated, literal) {
		t.Fatalf("template/literal divergence:\ntemplate: %v\nliteral:  %v", templated, literal)
	}
}

// When the recorder's chain declines classification (an aggregate
// argument the fused pass cannot express), no memo exists; the consumer
// falls back to its full filter and stays correct.
func TestScanCSERecorderDeclinedFallsBack(t *testing.T) {
	g := colAggFixture(t)
	q := `MATCH (m:Message) WHERE m.length < 150 RETURN count(m.length + 1) AS total
 NEXT MATCH (m:Message) WHERE m.length < 150 AND m.length > 20
 RETURN total, m.length AS l, count(m) AS n
 NEXT RETURN total, l, n ORDER BY l`
	rec0, cons0 := colAggCSERecorded, colAggCSEConsumed
	withMarks := cseRun(t, g, q, false)
	if colAggCSERecorded != rec0 || colAggCSEConsumed != cons0 {
		t.Fatalf("counters moved (recorded +%d, consumed +%d) on a declined recorder, want +0/+0",
			colAggCSERecorded-rec0, colAggCSEConsumed-cons0)
	}
	plan.DisableScanCSE = true
	defer func() { plan.DisableScanCSE = false }()
	if unmarked := cseRun(t, g, q, false); !slices.Equal(withMarks, unmarked) {
		t.Fatalf("ordered-row divergence:\nmarked:   %v\nunmarked: %v", withMarks, unmarked)
	}
	if len(withMarks) == 0 {
		t.Fatal("0 rows -- the fallback measured nothing")
	}
}
