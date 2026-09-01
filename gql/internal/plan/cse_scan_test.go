// Marking matrix for cross-segment scan CSE: the qualifying NEXT shape
// (a colagg scan whose conjuncts a later colagg scan of the same label
// and variable extends) pairs recorder and consumer, and every
// disqualifying feature refuses -- pinned at the plan so the exec
// identity test cannot pass vacuously on an unshared pipeline.
package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// cseMarks extracts (recorder index, consumer index) from a plan's first
// branch, -1 for absent; multiple marks of either kind fail the test.
func cseMarks(t *testing.T, p *Plan, src string) (int, int) {
	t.Helper()
	rec, cons := -1, -1
	for i, s := range p.Branches[0] {
		if s.CSERecord {
			if rec != -1 {
				t.Fatalf("%q: two recorder segments (%d, %d)", src, rec, i)
			}
			rec = i
		}
		if s.CSEFrom != nil {
			if cons != -1 {
				t.Fatalf("%q: two consumer segments (%d, %d)", src, cons, i)
			}
			cons = i
		}
	}
	return rec, cons
}

func TestScanCSEMarksSubsumingPair(t *testing.T) {
	g := buildFixture(t)
	src := "MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS total NEXT MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN total, count(m) AS c"
	p := mustPlan(t, g, src)
	rec, cons := cseMarks(t, p, src)
	if rec != 0 || cons != 1 {
		t.Fatalf("marks = (rec %d, cons %d), want (0, 1)", rec, cons)
	}
	seg := p.Branches[0][1]
	if seg.CSEFrom != p.Branches[0][0] {
		t.Fatalf("consumer points at %p, want segment 0 (%p)", seg.CSEFrom, p.Branches[0][0])
	}
	if seg.CSEResidual == nil {
		t.Fatal("consumer residual is nil, want the unshared conjunct (m.len > 20)")
	}
	if len(seg.CSEParamConds) != 0 {
		t.Fatalf("literal plan carries param conds %v, want none", seg.CSEParamConds)
	}
}

// An identical filter shares with an empty residual: the consumer's pass
// runs no conjuncts at all over the memo.
func TestScanCSEIdenticalFilterEmptyResidual(t *testing.T) {
	g := buildFixture(t)
	src := "MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS total NEXT MATCH (m:Message) WHERE m.len < 100 RETURN total, count(m) AS c"
	p := mustPlan(t, g, src)
	rec, cons := cseMarks(t, p, src)
	if rec != 0 || cons != 1 {
		t.Fatalf("marks = (rec %d, cons %d), want (0, 1)", rec, cons)
	}
	if res := p.Branches[0][1].CSEResidual; res != nil {
		t.Fatalf("residual = %v, want nil for identical filters", res)
	}
}

func TestScanCSEDeclines(t *testing.T) {
	g := buildFixture(t)
	for _, tc := range []struct {
		name, src string
	}{
		{"different-label",
			"MATCH (m:Person) WHERE m.pid < 5 RETURN count(m) AS t NEXT MATCH (m:Message) WHERE m.pid < 5 AND m.len > 0 RETURN t, count(m) AS c"},
		{"different-variable",
			"MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS t NEXT MATCH (x:Message) WHERE x.len < 100 AND x.len > 20 RETURN t, count(x) AS c"},
		{"non-subset-bound",
			"MATCH (m:Message) WHERE m.len < 50 RETURN count(m) AS t NEXT MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN t, count(m) AS c"},
		{"recorder-unfiltered",
			"MATCH (m:Message) RETURN count(m) AS t NEXT MATCH (m:Message) WHERE m.len > 20 RETURN t, count(m) AS c"},
		{"recorder-superset",
			"MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN count(m) AS t NEXT MATCH (m:Message) WHERE m.len < 100 RETURN t, count(m) AS c"},
		{"row-dependent-recorder-conjunct",
			"MATCH (tg:Tag) RETURN count(tg) AS n NEXT MATCH (m:Message) WHERE m.len < n RETURN n, count(m) AS c NEXT MATCH (m:Message) WHERE m.len < n AND m.len > 2 RETURN c, count(m) AS c2"},
		{"optional-consumer",
			"MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS t NEXT OPTIONAL MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN t, count(m) AS c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPlan(t, g, tc.src)
			if rec, cons := cseMarks(t, p, tc.src); rec != -1 || cons != -1 {
				t.Fatalf("marks = (rec %d, cons %d), want none", rec, cons)
			}
		})
	}
}

func TestScanCSEDisableSwitch(t *testing.T) {
	g := buildFixture(t)
	DisableScanCSE = true
	defer func() { DisableScanCSE = false }()
	src := "MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS total NEXT MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN total, count(m) AS c"
	p := mustPlan(t, g, src)
	if rec, cons := cseMarks(t, p, src); rec != -1 || cons != -1 {
		t.Fatalf("marks under DisableScanCSE = (rec %d, cons %d), want none", rec, cons)
	}
}

// An auto-parameterized template lifts each comparison bound to its own
// slot, so the shared conjunct matches conditionally: the pair must be
// recorded for the executor to validate per run.
func TestScanCSEParamConds(t *testing.T) {
	g := buildFixture(t)
	src := "MATCH (m:Message) WHERE m.len < 100 RETURN count(m) AS total NEXT MATCH (m:Message) WHERE m.len < 100 AND m.len > 20 RETURN total, count(m) AS c"
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	vals := semantics.AutoParameterize(q)
	if len(vals) != 3 {
		t.Fatalf("lifted %d values, want 3 (two bounds in the consumer, one in the recorder)", len(vals))
	}
	p, err := Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rec, cons := cseMarks(t, p, src)
	if rec != 0 || cons != 1 {
		t.Fatalf("template marks = (rec %d, cons %d), want (0, 1)", rec, cons)
	}
	conds := p.Branches[0][1].CSEParamConds
	if len(conds) != 1 || conds[0] != [2]uint32{0, 1} {
		t.Fatalf("param conds = %v, want [[0 1]] (recorder bound slot vs consumer's copy)", conds)
	}
}
