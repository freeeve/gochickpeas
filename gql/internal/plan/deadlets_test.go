// Marking matrix for the dead-work boundary reductions: the
// size-of-unfiltered-comprehension rewrite and dead-LET nulling fire on
// their shapes and refuse everything else -- pinned at the plan so the
// e2e differential cannot pass vacuously.
package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func planDeltas(t *testing.T, src string) (rewrites, nullings int, p *Plan) {
	t.Helper()
	g := buildFixture(t)
	r0, n0 := compLenRewrites, deadLetNullings
	p = mustPlan(t, g, src)
	return compLenRewrites - r0, deadLetNullings - n0, p
}

func TestDeadLetReductionsFire(t *testing.T) {
	// The trail idiom's post-mono shape: an unfiltered comprehension
	// bound at a boundary, consumed only via size() downstream.
	// A FILTER boundary between the definition and the aggregate forces
	// the cross-segment shape the trail idiom has post-mono-pushdown.
	src := "MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e | r.since] FILTER f.pid >= 0 RETURN f.pid AS pid, min(size(ts)) AS d ORDER BY pid"
	rw, nl, pl := planDeltas(t, src)
	if rw < 1 {
		t.Fatalf("size rewrite fired %d, want >= 1", rw)
	}
	if nl < 1 {
		t.Fatalf("dead-LET nulling fired %d, want >= 1 (ts unread after the rewrite)", nl)
	}
	// The DEFINING item is nulled in place (column position kept); the
	// downstream passthrough stays a bare Var carrying the null. No
	// comprehension survives anywhere.
	nulled := false
	for _, seg := range pl.Branches[0][:len(pl.Branches[0])-1] {
		for _, it := range seg.Proj.Returns {
			if it.Name != "ts" {
				continue
			}
			switch e := it.Expr.(type) {
			case *ast.Lit:
				if e.Value.Kind == ast.LitNull {
					nulled = true
				}
			case *ast.ListComp:
				t.Fatal("the comprehension survived the nulling")
			}
		}
	}
	if !nulled {
		t.Fatal("no ts item was nulled")
	}
}

func TestDeadLetReductionsDecline(t *testing.T) {
	for _, tc := range []struct {
		name, src        string
		wantRw, wantNull int
	}{
		// A FILTERED comprehension must not length-rewrite (and stays
		// live through its size read).
		{"filtered-comprehension",
			"MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e WHERE r.since > 0 | r.since] RETURN f.pid AS pid, min(size(ts)) AS d ORDER BY pid", 0, 0},
		// ts read beyond size(): stays computed (nulling must refuse;
		// the size reads themselves may still rewrite, count unpinned).
		{"element-read",
			"MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e | r.since] FILTER ts[0] > 0 RETURN f.pid AS pid, min(size(ts)) AS d ORDER BY pid", -1, 0},
		// The comprehension feeding a FINAL output survives.
		{"final-output",
			"MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e | r.since] RETURN f.pid AS pid, ts AS ts ORDER BY pid", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rw, nl, _ := planDeltas(t, tc.src)
			if tc.wantRw >= 0 && rw != tc.wantRw {
				t.Fatalf("rewrites = %d, want %d", rw, tc.wantRw)
			}
			if nl != tc.wantNull {
				t.Fatalf("nullings = %d, want %d", nl, tc.wantNull)
			}
		})
	}
}

func TestDeadLetSourceRedefinitionGuard(t *testing.T) {
	// The source name is redefined between the definition and the read:
	// size(ts) must NOT redirect to the new value's name.
	src := "MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e | r.since] FILTER f.pid >= 0 LET e = [1] FILTER f.pid >= 0 RETURN f.pid AS pid, min(size(ts)) AS d ORDER BY pid"
	rw, _, _ := planDeltas(t, src)
	if rw != 0 {
		t.Fatalf("size rewrite fired %d across a source redefinition, want 0", rw)
	}
}

func TestDeadLetDisableSwitch(t *testing.T) {
	DisableDeadLets = true
	defer func() { DisableDeadLets = false }()
	rw, nl, _ := planDeltas(t,
		"MATCH (p:Person)-[e:KNOWS]->{1,2}(f:Person) LET ts = [r IN e | r.since] FILTER f.pid >= 0 RETURN f.pid AS pid, min(size(ts)) AS d ORDER BY pid")
	if rw != 0 || nl != 0 {
		t.Fatalf("reductions fired under DisableDeadLets (%d, %d)", rw, nl)
	}
}
