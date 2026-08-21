// Fusion matrix for the sort-limit hoist: the qualifying NEXT shape
// donates its ORDER BY / OFFSET / LIMIT to the producing segment, and
// every disqualifying feature refuses -- pinned at the plan so the exec
// identity test cannot pass vacuously on an unfused pipeline.
package plan

import "testing"

// orderCarrier finds the single segment carrying an ORDER BY, failing
// on zero or several (the fixtures author exactly one ordering).
func orderCarrier(t *testing.T, p *Plan, src string) (int, *Segment) {
	t.Helper()
	idx, found := -1, 0
	for i, s := range p.Branches[0] {
		if len(s.Proj.OrderBy) > 0 {
			idx = i
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%q: %d segments carry ORDER BY, want exactly 1", src, found)
	}
	return idx, p.Branches[0][idx]
}

func TestFuseSortLimit(t *testing.T) {
	g := buildFixture(t)

	// The IC9 shape: plain producer, trailing order-limit projection.
	// After fusion the ordering lives on the segment WITH stages and the
	// trailer keeps neither ordering nor bound.
	src := "MATCH (p:Person) RETURN p.pid AS pid, p.name AS nm NEXT ORDER BY pid DESC, nm ASC LIMIT 3 RETURN nm"
	p := mustPlan(t, g, src)
	i, carrier := orderCarrier(t, p, src)
	if len(carrier.Stages) == 0 || carrier.Proj.Limit == nil || *carrier.Proj.Limit != 3 {
		t.Fatalf("order carrier is segment %d (stages=%d, limit=%v), want the staged producer with limit 3",
			i, len(carrier.Stages), carrier.Proj.Limit)
	}

	// OFFSET travels with the limit.
	src = "MATCH (p:Person) RETURN p.pid AS pid NEXT ORDER BY pid SKIP 2 LIMIT 3 RETURN pid"
	_, carrier = orderCarrier(t, mustPlan(t, g, src), src)
	if len(carrier.Stages) == 0 || carrier.Proj.Skip == nil || *carrier.Proj.Skip != 2 {
		t.Fatal("offset did not travel to the staged producer with the fused limit")
	}

	// An aggregated producer receives too (finalize handles the bound).
	src = "MATCH (p:Person) RETURN p.name AS nm, count(*) AS n NEXT ORDER BY n DESC LIMIT 2 RETURN nm, n"
	_, carrier = orderCarrier(t, mustPlan(t, g, src), src)
	if !carrier.Proj.Aggregated || carrier.Proj.Limit == nil {
		t.Fatal("aggregated producer did not receive the sort-limit")
	}

	// Refusals: the ordering must remain on its authored stage-less
	// segment (or its authored producer for the last two).
	refusals := []struct {
		src        string
		carrierHas string // "stageless" = must stay on a no-stage segment; "producer" = authored there
	}{
		{"MATCH (p:Person) RETURN p.pid AS pid NEXT MATCH (q:Person) RETURN pid, q.pid AS qp ORDER BY pid LIMIT 3", "staged-own"},
		{"MATCH (p:Person) RETURN p.pid AS pid NEXT RETURN DISTINCT pid ORDER BY pid LIMIT 3", "stageless"},
		// DISTINCT producer keeps the boundary out of the AST-level
		// trailing-projection fusion, so the aggregating trailer's
		// refusal is actually exercised.
		{"MATCH (p:Person) RETURN DISTINCT p.pid AS pid NEXT RETURN count(*) AS n ORDER BY n LIMIT 3", "stageless"},
		{"MATCH (p:Person) RETURN p.pid AS pid NEXT ORDER BY pid % 7 LIMIT 3 RETURN pid", "stageless"},
	}
	for _, tc := range refusals {
		_, carrier := orderCarrier(t, mustPlan(t, g, tc.src), tc.src)
		switch tc.carrierHas {
		case "stageless":
			if len(carrier.Stages) != 0 {
				t.Errorf("%q fused onto a staged segment but must refuse", tc.src)
			}
		case "staged-own":
			// The trailer authored its own MATCH; the ordering stays with
			// that trailer (which has stages), NOT the first producer.
			if len(carrier.Stages) == 0 {
				t.Errorf("%q: ordering left its authoring segment", tc.src)
			}
		}
	}

	// No-limit trailer: nothing to gain, ordering stays put.
	src = "MATCH (p:Person) RETURN p.pid AS pid NEXT ORDER BY pid RETURN pid"
	_, carrier = orderCarrier(t, mustPlan(t, g, src), src)
	if len(carrier.Stages) != 0 {
		t.Error("no-limit trailer fused but must refuse")
	}

	// Producer already ordered/limited: its own ordering survives and the
	// trailer keeps its bound (two carriers -> checked directly).
	for _, src := range []string{
		"MATCH (p:Person) RETURN p.pid AS pid ORDER BY pid NEXT ORDER BY pid DESC LIMIT 3 RETURN pid",
		"MATCH (p:Person) RETURN p.pid AS pid LIMIT 5 NEXT ORDER BY pid LIMIT 3 RETURN pid",
	} {
		p := mustPlan(t, g, src)
		last := -1
		for i, s := range p.Branches[0] {
			if len(s.Proj.OrderBy) > 0 && s.Proj.Limit != nil {
				last = i
			}
		}
		if last < 0 || len(p.Branches[0][last].Stages) != 0 {
			t.Errorf("%q: trailer's order+limit moved off its stage-less segment", src)
		}
	}
}
