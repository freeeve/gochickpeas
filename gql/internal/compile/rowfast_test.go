// Parity tests for the whole-row predicate specialization: the fast form
// must agree with the interpreter on every shape it claims -- prop-vs-prop
// comparisons with and without constant integer/duration shifts, slot
// comparisons -- across absent properties, null and non-node slot values,
// overflow, and reversed operand order.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func TestRowFastMatchesInterpreter(t *testing.T) {
	b := chickpeas.NewBuilder(16, 4)
	var ids []chickpeas.NodeID
	for i := range 6 {
		n, _ := b.AddNode("N")
		ids = append(ids, n)
		switch i {
		case 0, 1, 2, 3:
			_ = b.SetProp(n, "ts", int64(1_000_000+i*7_200_000)) // 2h apart
			_ = b.SetProp(n, "k", int64(i*10))
		case 4:
			_ = b.SetProp(n, "ts", int64(9_223_372_036_854_775_000)) // near MaxInt64
			// 5: all props missing
		}
	}
	g := b.Finalize("rowfast")
	ctx := &eval.Ctx{G: graph.New(g)}
	slots := map[string]int{"a": 0, "b": 1}

	exprs := []string{
		"a.ts > b.ts", "a.ts < b.ts", "a.ts = b.ts", "a.ts <> b.ts",
		"a.ts >= b.ts", "a.ts <= b.ts",
		"a.ts > b.ts + duration({hours: 4})",
		"a.ts <= b.ts - duration({minutes: 30})",
		"a.ts > duration({days: 1}) + b.ts",
		"a.ts + duration({hours: 1}) < b.ts + duration({hours: 2})",
		"a.k + 15 > b.k", "a.k < b.k - 5", "10 + a.k >= b.k",
		"a.k + 9223372036854775800 > b.k", // checked-add overflow -> Null
		"a = b", "a <> b", "a < b",
	}
	for _, src := range exprs {
		e := exprOf(t, src)
		c := New(ctx, e, slots, g)
		if c.fast == nil {
			t.Fatalf("%q did not derive a fast form (compiled to %T)", src, c.c)
		}
		for _, ia := range ids {
			for _, ib := range ids {
				row := []value.Value{value.Node(graph.NodeID(ia)), value.Node(graph.NodeID(ib))}
				got := c.Eval(ctx, row, slots)
				want := eval.Eval(ctx, e, row, slots)
				if !value.Identical(got, want) {
					t.Fatalf("%q a=%d b=%d: fast %v, interp %v", src, ia, ib, got, want)
				}
			}
		}
		// Null, non-node, and short-row inputs must match too (the fast
		// form falls back or propagates Null).
		for _, row := range [][]value.Value{
			{value.Null(), value.Node(graph.NodeID(ids[0]))},
			{value.Node(graph.NodeID(ids[0])), value.Null()},
			{value.Int(42), value.Node(graph.NodeID(ids[1]))},
			{value.Node(graph.NodeID(ids[1])), value.Str("x")},
			{value.Node(graph.NodeID(ids[0]))},
		} {
			got := c.Eval(ctx, row, slots)
			want := eval.Eval(ctx, e, row, slots)
			if !value.Identical(got, want) {
				t.Fatalf("%q row %v: fast %v, interp %v", src, row, got, want)
			}
		}
	}

	// Shapes that must NOT specialize keep the tree evaluation.
	for _, src := range []string{
		"a.ts > b.ts + duration({months: 1})", // calendar-dependent shift
		"a.ts + 1.5 > b.ts",                   // float shift
		"a.ts + b.k > 3",                      // two-prop arithmetic side
		"a.ts > 1.5",                          // FLOAT literal: must decline (truncation would change the result)
		"1.5 < a.ts",                          // float literal on the left
		"a.name = 'x'",                        // string literal: not an integer compare
	} {
		c := New(ctx, exprOf(t, src), slots, g)
		if c.fast != nil {
			t.Fatalf("%q unexpectedly derived a fast form", src)
		}
	}
}

// TestRowFastPropVsLiteral is the prop-vs-LITERAL differential matrix (task
// 102): every operator x integer/temporal literal x operand order x a possibly
// shifted term must derive a fast form whose per-row answer is bit-identical to
// the interpreter, over the same absent/null/non-node/short-row inputs. Integer
// literals compare through the float64 asNum path; temporal literals fold to
// epoch millis and compare exactly (cmpInt) -- both must mirror value.Compare.
func TestRowFastPropVsLiteral(t *testing.T) {
	b := chickpeas.NewBuilder(16, 4)
	var ids []chickpeas.NodeID
	for i := range 6 {
		n, _ := b.AddNode("N")
		ids = append(ids, n)
		switch i {
		case 0, 1, 2, 3:
			_ = b.SetProp(n, "ts", int64(1_311_120_000_000+int64(i)*86_400_000)) // ~2011-07 epoch millis, 1 day apart
			_ = b.SetProp(n, "k", int64(i*10))
		case 4:
			_ = b.SetProp(n, "k", int64(9_223_372_036_854_775_000)) // near MaxInt64
			// 5: all props missing
		}
	}
	g := b.Finalize("rowfast_lit")
	ctx := &eval.Ctx{G: graph.New(g)}
	slots := map[string]int{"a": 0}

	exprs := []string{
		// integer literal, both operand orders, every operator
		"a.k > 20", "a.k < 20", "a.k = 20", "a.k <> 20", "a.k >= 20", "a.k <= 20",
		"20 > a.k", "20 < a.k", "20 = a.k", "20 <= a.k",
		// shifted term vs integer literal
		"a.k + 5 > 20", "a.k - 5 < 20", "10 + a.k >= 20",
		// temporal literal (folds to epoch millis, exact int64 compare)
		"a.ts < datetime('2011-07-25')", "a.ts >= datetime('2011-07-25')",
		"datetime('2011-07-25') > a.ts", "a.ts = datetime('2011-07-25')",
		// shifted temporal term vs temporal literal
		"a.ts + duration({hours: 4}) < datetime('2011-07-25')",
	}
	for _, src := range exprs {
		e := exprOf(t, src)
		c := New(ctx, e, slots, g)
		if c.fast == nil {
			t.Fatalf("%q did not derive a fast form (compiled to %T)", src, c.c)
		}
		for _, ia := range ids {
			row := []value.Value{value.Node(graph.NodeID(ia))}
			got := c.Eval(ctx, row, slots)
			want := eval.Eval(ctx, e, row, slots)
			if !value.Identical(got, want) {
				t.Fatalf("%q a=%d: fast %v, interp %v", src, ia, got, want)
			}
		}
		for _, row := range [][]value.Value{
			{value.Null()},
			{value.Int(42)},
			{value.Str("x")},
			{},
		} {
			got := c.Eval(ctx, row, slots)
			want := eval.Eval(ctx, e, row, slots)
			if !value.Identical(got, want) {
				t.Fatalf("%q row %v: fast %v, interp %v", src, row, got, want)
			}
		}
	}
}
