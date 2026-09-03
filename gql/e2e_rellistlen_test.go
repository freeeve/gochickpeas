// Rel-list length elision end-to-end: the count-bound execution against
// the materialized-list pipeline, row-for-row in engine order over the
// descending-timestamp trail idiom (the CR1 shape) and a plain
// size-of-rel-list projection, duplicate and rejected trails included.
package gql_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// relLenBoth runs q with and without the elision and asserts identical
// ordered rows, returning them.
func relLenBoth(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	elided := hjRowKeysOrdered(t, g, q)
	plan.DisableRelLenOnly = true
	defer func() { plan.DisableRelLenOnly = false }()
	materialized := hjRowKeysOrdered(t, g, q+" ")
	if !slices.Equal(elided, materialized) {
		t.Fatalf("ordered-row divergence on %s:\ncount-bound  (%d): %v\nmaterialized (%d): %v",
			q, len(elided), elided, len(materialized), materialized)
	}
	return elided
}

func TestRelLenOnlyTrailIdiom(t *testing.T) {
	g := elideGraph(t)
	// The CR1 shape end-to-end; the elision stacks on the path elision
	// and the dead-LET reductions.
	rows := relLenBoth(t, g,
		"MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) LET ts = [r IN rels(p) | r.ct] FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) RETURN o.aid AS oid, min(size(ts)) AS dist ORDER BY oid")
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
}

func TestRelLenOnlyDirectSize(t *testing.T) {
	g := elideGraph(t)
	q := "MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) RETURN o.aid AS oid, size(e) AS n ORDER BY oid, n"
	relLenBoth(t, g, q)
	if got := intCol(t, g, q, "n"); len(got) == 0 || slices.Max(got) < 2 {
		t.Fatalf("expected multi-hop trails in %v", got)
	}
}

func TestRelLenOnlyElementReadStillMaterializes(t *testing.T) {
	g := elideGraph(t)
	// An element read declines the elision; both legs must still agree
	// (this pins the decline path's correctness, not just the counter).
	relLenBoth(t, g,
		"MATCH (s:A {aid: 0})<-[e:T]-{1,3}(o:A) LET ts = [r IN e | r.ct] RETURN o.aid AS oid, size(ts) AS n ORDER BY oid, n")
}
