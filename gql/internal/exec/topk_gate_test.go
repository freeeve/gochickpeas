// Payload-gated top-k oracle (task 117): under ORDER BY <col> LIMIT k the
// sink must build the projected payload only for rows the bounded heap
// would keep -- the assertion is a BUILD COUNT, not a duration. Ascending
// input under ASC means the first k rows fill the heap and every later
// candidate is refused on one key comparison: exactly k builds.
package exec

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func topkFixture(t *testing.T, n int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, 1)
	for i := range n {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "v", int64(i)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "name", fmt.Sprintf("n%04d", i)); err != nil {
			t.Fatal(err)
		}
		// tie groups of 10 for the boundary-stability case
		if err := b.SetProp(nd, "grp", int64(i/10)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("topk")
}

// runGateBoth runs q with the payload gate on, then off, returning both
// row lists -- the true differential (runBoth's knob is colagg's).
func runGateBoth(t *testing.T, g *chickpeas.Snapshot, q string) (gated, ungated []string) {
	t.Helper()
	disableTopkGate = false
	gated, _ = runBoth(t, g, q)
	disableTopkGate = true
	ungated, _ = runBoth(t, g, q)
	disableTopkGate = false
	return gated, ungated
}

func TestTopKPayloadGate(t *testing.T) {
	g := topkFixture(t, 500)
	before := topkPayloadBuilds
	disableTopkGate = false
	rows, _ := runBoth(t, g,
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 5")
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	builds := topkPayloadBuilds - before
	// runBoth executes twice, so each pass builds exactly 5 payloads:
	// ascending input under ASC fills the heap with the first 5 and
	// refuses all 495 others on one key comparison. ~1000 means ungated.
	if builds != 10 {
		t.Fatalf("payload builds = %d across two runs, want 10 (5 per run; ~1000 means ungated)", builds)
	}
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v DESC LIMIT 7")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("gated path diverged from unguarded:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestTopKGateTieAtBoundary pins stability where a key tie straddles the
// LIMIT boundary: grp 0 has ten rows and LIMIT cuts at six, so which six
// survive is decided by arrival order -- the gate must refuse tied
// late-arrivals exactly as offer-then-pop would.
func TestTopKGateTieAtBoundary(t *testing.T) {
	g := topkFixture(t, 100)
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.grp AS grp, m.name AS name ORDER BY grp ASC LIMIT 6")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("tie-at-boundary diverged:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestTopKCompositeKeyUngated pins the unguarded path: an ORDER BY
// expression that is not a bare output column needs the whole row, so it
// must keep the build-then-offer flow and still agree with the general
// sort.
func TestTopKCompositeKeyUngated(t *testing.T) {
	g := topkFixture(t, 100)
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.name AS name ORDER BY m.v + 1 DESC LIMIT 4")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("composite-key path diverged:\n%v\nvs\n%v", gated, ungated)
	}
}
