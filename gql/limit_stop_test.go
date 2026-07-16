// The stop protocol: an unordered LIMIT that has its k rows tells the
// producers above it to abandon the walk (rowSink.push returning false),
// so MATCH (n:Label) RETURN n LIMIT 1 binds O(1) scan candidates instead
// of every node. The tests observe WORK (PROFILE's produced-row
// counters), not results -- row-level correctness is covered by the
// existing suites -- and pin the protocol's necessary asymmetry: ORDER BY
// and aggregation must still consume everything.
package gql

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// bigScanGraph is n :N nodes with an i64 v property.
func bigScanGraph(t *testing.T, n int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, 1)
	for i := 0; i < n; i++ {
		id, _ := b.AddNode("N")
		_ = b.SetProp(id, "v", int64(i))
	}
	return b.Finalize()
}

// scanCount extracts the produced-row count from the plan's NodeScan line.
func scanCount(t *testing.T, p string) int {
	t.Helper()
	l := lineWith(p, "NodeScan")
	if l == "" {
		t.Fatalf("no NodeScan line:\n%s", p)
	}
	fs := strings.Fields(l)
	c, err := strconv.Atoi(strings.ReplaceAll(fs[len(fs)-1], ",", ""))
	if err != nil {
		t.Fatalf("unparsable NodeScan count in %q: %v", l, err)
	}
	return c
}

// TestLimitStopsScan pins the stop protocol: LIMIT k without ORDER BY
// binds ~k scan candidates, never the whole label.
func TestLimitStopsScan(t *testing.T) {
	const n = 10000
	g := bigScanGraph(t, n)
	for _, k := range []int{1, 5} {
		p := planText(t, g, fmt.Sprintf("PROFILE MATCH (x:N) RETURN x LIMIT %d", k))
		if c := scanCount(t, p); c > k {
			t.Fatalf("LIMIT %d bound %d scan candidates (want <= %d): the stop protocol is not terminating the scan\n%s", k, c, k, p)
		}
	}
	// DISTINCT + LIMIT stops too: the first k distinct rows are final in
	// arrival order, so later candidates cannot change the result.
	p := planText(t, g, "PROFILE MATCH (x:N) RETURN DISTINCT x.v AS v LIMIT 3")
	if c := scanCount(t, p); c > 3 {
		t.Fatalf("DISTINCT LIMIT 3 bound %d scan candidates (want <= 3)\n%s", c, p)
	}
}

// TestLimitStopAsymmetry pins what must NOT stop: ORDER BY needs the
// total order and aggregation needs every row, so both consume the whole
// scan even under LIMIT.
func TestLimitStopAsymmetry(t *testing.T) {
	const n = 10000
	g := bigScanGraph(t, n)
	p := planText(t, g, "PROFILE MATCH (x:N) RETURN x.v AS v ORDER BY v DESC LIMIT 1")
	if c := scanCount(t, p); c != n {
		t.Fatalf("ORDER BY LIMIT scanned %d of %d: an ordered LIMIT must consume everything", c, n)
	}
	p = planText(t, g, "PROFILE MATCH (x:N) RETURN count(*) AS c")
	if c := scanCount(t, p); c != n {
		t.Fatalf("aggregation scanned %d of %d: aggregation must consume everything", c, n)
	}
}
