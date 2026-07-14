// Plan-time constant folding must stay sound for zero-argument scalar
// functions. A fold site decides "evaluate at plan time?" by asking whether
// every argument is constant; a zero-arg call passes that test vacuously and
// folds -- correct ONLY while every such function is deterministic. If a
// volatile zero-arg function (rand()/timestamp()/now()) is ever added, the
// baked plan-time value would be cached and replayed forever, so it must be
// excluded from the fold at foldFunc AND constExpr both. These tests lock the
// current invariant: gochickpeas has no volatile scalar function, and the
// only zero-arg forms -- date()/datetime()/localdatetime() -- are
// deterministic (they return Null, not the current instant). They fail loudly
// the day a zero-arg function stops being deterministic without its fold
// being guarded (rustychickpeas twin 9735204).
package gql

import "testing"

// TestZeroArgTemporalFunctionsAreDeterministic locks that the only zero-arg
// scalar functions are non-volatile, so the vacuous plan-time fold of a
// zero-arg call stays sound (task 083).
func TestZeroArgTemporalFunctionsAreDeterministic(t *testing.T) {
	g := socialGraph(t)
	anchor := "MATCH (p:Person {name: 'Alice'}) RETURN "
	for _, fn := range []string{"date()", "datetime()", "localdatetime()"} {
		v, n := scalarVal(t, g, anchor+fn+" AS d", "d")
		if n != 1 || !v.IsNull() {
			t.Fatalf("%s: rows=%d value=%v (want 1 row, Null -- a volatile 'current instant' form here would poison the cached plan)", fn, n, v)
		}
	}
}

// TestZeroArgFoldStableAcrossExecutions guards the baked-plan hazard directly:
// a zero-arg function folded into the plan must yield the SAME value on a
// second execution, never a value frozen from the first. Trivially true today
// (deterministic Null) and the tripwire that a future volatile addition trips.
func TestZeroArgFoldStableAcrossExecutions(t *testing.T) {
	g := socialGraph(t)
	q := "MATCH (p:Person {name: 'Alice'}) RETURN datetime() AS d"
	first, n1 := scalarVal(t, g, q, "d")
	second, n2 := scalarVal(t, g, q, "d")
	if n1 != 1 || n2 != 1 {
		t.Fatalf("row counts: first=%d second=%d (want 1 each)", n1, n2)
	}
	if !first.IsNull() || !second.IsNull() || first.Kind() != second.Kind() {
		t.Fatalf("zero-arg fold not stable/deterministic: first=%v second=%v", first, second)
	}
}

// TestZeroArgFoldStableThroughPlanCache runs the same zero-arg query twice
// through the plan cache -- where the fold hazard actually bites (one plan is
// built once and replayed). Both executions must agree.
func TestZeroArgFoldStableThroughPlanCache(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(1 << 20)
	q := "MATCH (p:Person {name: 'Alice'}) RETURN datetime() AS d"
	run := func() (nullVal bool, rows int) {
		res, err := c.Run(g, q)
		if err != nil {
			t.Fatalf("cached run failed: %v", err)
		}
		for r := range res.All() {
			rows++
			v, _ := r.Get("d")
			nullVal = v.IsNull()
		}
		return
	}
	n1, r1 := run()
	n2, r2 := run()
	if r1 != 1 || r2 != 1 || !n1 || !n2 {
		t.Fatalf("cached zero-arg fold: run1=(null=%v,rows=%d) run2=(null=%v,rows=%d), want deterministic Null both", n1, r1, n2, r2)
	}
}
