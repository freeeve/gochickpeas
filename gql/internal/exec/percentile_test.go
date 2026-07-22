package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// pctConstEval is a RowEval that ignores its row and returns a fixed value:
// the percentile argument fed to percentileOf in these tests.
type pctConstEval struct{ v value.Value }

func (c pctConstEval) Eval(*eval.Ctx, []value.Value, map[string]int) value.Value { return c.v }

// TestPercentileOfGuards covers the Null-returning guards: no expression,
// an empty group, a percentile outside [0,1], and a non-numeric percentile.
func TestPercentileOfGuards(t *testing.T) {
	ctx := &eval.Ctx{}
	vals := []value.Value{value.Int(10), value.Int(20)}
	if got := percentileOf(ctx, nil, vals, false); !got.IsNull() {
		t.Fatalf("nil pc = %v, want null", got)
	}
	if got := percentileOf(ctx, pctConstEval{value.Float(0.5)}, nil, false); !got.IsNull() {
		t.Fatalf("empty group = %v, want null", got)
	}
	for _, p := range []float64{-0.1, 1.5} {
		if got := percentileOf(ctx, pctConstEval{value.Float(p)}, []value.Value{value.Int(1)}, true); !got.IsNull() {
			t.Fatalf("p=%v = %v, want null", p, got)
		}
	}
	if got := percentileOf(ctx, pctConstEval{value.Str("half")}, []value.Value{value.Int(1)}, true); !got.IsNull() {
		t.Fatalf("non-float p = %v, want null", got)
	}
}

// unsorted returns a fresh out-of-order group; percentileOf sorts in place,
// so each call gets its own copy.
func unsorted() []value.Value {
	return []value.Value{value.Int(30), value.Int(10), value.Int(40), value.Int(20)}
}

// TestPercentileDisc covers PERCENTILE_DISC nearest-rank selection
// (ceil(p*n) clamped to [1,n], 1-based) returning the collected value with
// its kind unchanged, and that the group is sorted first.
func TestPercentileDisc(t *testing.T) {
	ctx := &eval.Ctx{}
	for _, c := range []struct {
		p    float64
		want int64
	}{{0.0, 10}, {0.25, 10}, {0.5, 20}, {0.75, 30}, {1.0, 40}} {
		got := percentileOf(ctx, pctConstEval{value.Float(c.p)}, unsorted(), false)
		if got.Kind() != value.KindInt {
			t.Fatalf("disc p=%v kind = %v, want Int (unchanged)", c.p, got.Kind())
		}
		if iv, _ := got.AsInt(); iv != c.want {
			t.Fatalf("disc p=%v = %v, want %d", c.p, got, c.want)
		}
	}
}

// TestPercentileCont covers PERCENTILE_CONT linear interpolation between the
// two straddling values over the sorted group, always returning Float.
func TestPercentileCont(t *testing.T) {
	ctx := &eval.Ctx{}
	// Sorted group is [10,20,30,40], n-1 = 3; rank = p*3.
	for _, c := range []struct {
		p    float64
		want float64
	}{{0.0, 10}, {0.25, 17.5}, {0.5, 25}, {1.0, 40}} {
		got := percentileOf(ctx, pctConstEval{value.Float(c.p)}, unsorted(), true)
		if got.Kind() != value.KindFloat {
			t.Fatalf("cont p=%v kind = %v, want Float", c.p, got.Kind())
		}
		if fv, _ := got.AsFloat(); fv != c.want {
			t.Fatalf("cont p=%v = %v, want %v", c.p, got, c.want)
		}
	}
}
