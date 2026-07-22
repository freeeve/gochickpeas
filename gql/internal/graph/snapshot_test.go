package graph

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// Coverage for the Snapshot adapter accessors the other tests do not
// exercise: the degree/label statistics, the matcher-driven append and
// count traversal accessors on the hot walk path, and the chain/functional
// capability resolvers.

// knowsPos returns the CSR position of the from-KNOWS->to relationship, or
// fails the test when it is absent.
func knowsPos(t *testing.T, s *SnapshotGraph, from, to chickpeas.NodeID) uint32 {
	t.Helper()
	for n, p := range s.Relationships(from, chickpeas.Outgoing, []string{"KNOWS"}) {
		if n == to {
			return p
		}
	}
	t.Fatalf("no KNOWS rel %d->%d", from, to)
	return 0
}

// TestDegreeAndLabelNames covers the O(1) degree accessor over both
// directions and the label listing.
func TestDegreeAndLabelNames(t *testing.T) {
	s := buildGraph(t)
	// alice(0): KNOWS->bob + LIVES_IN->city out; nothing in.
	if d := s.Degree(0, chickpeas.Outgoing); d != 2 {
		t.Fatalf("alice out-degree = %d, want 2", d)
	}
	if d := s.Degree(0, chickpeas.Incoming); d != 0 {
		t.Fatalf("alice in-degree = %d, want 0", d)
	}
	// bob(1): KNOWS->carol out; KNOWS<-alice in.
	if d := s.Degree(1, chickpeas.Outgoing); d != 1 {
		t.Fatalf("bob out-degree = %d, want 1", d)
	}
	if d := s.Degree(1, chickpeas.Incoming); d != 1 {
		t.Fatalf("bob in-degree = %d, want 1", d)
	}
	// carol(2): only KNOWS<-bob in. city(3): only LIVES_IN<-alice in.
	if d := s.Degree(2, chickpeas.Outgoing); d != 0 {
		t.Fatalf("carol out-degree = %d, want 0", d)
	}
	if d := s.Degree(3, chickpeas.Incoming); d != 1 {
		t.Fatalf("city in-degree = %d, want 1", d)
	}
	names := s.LabelNames()
	if len(names) != 2 || !slices.Contains(names, "Person") || !slices.Contains(names, "City") {
		t.Fatalf("label names = %v, want Person+City", names)
	}
}

// TestAvgDegreeByLabel covers the label-conditional degree statistic and its
// unknown-label miss.
func TestAvgDegreeByLabel(t *testing.T) {
	s := buildGraph(t)
	avg, ok := s.AvgDegreeByLabel("Person", "KNOWS", chickpeas.Outgoing)
	if !ok {
		t.Fatal("Person/KNOWS/out should resolve")
	}
	// alice + bob each have one outgoing KNOWS, carol none: a positive
	// average no greater than one per Person.
	if avg <= 0 || avg > 1 {
		t.Fatalf("avg KNOWS out-degree by Person = %v, want (0,1]", avg)
	}
	if _, ok := s.AvgDegreeByLabel("Nope", "KNOWS", chickpeas.Outgoing); ok {
		t.Fatal("unknown label must miss")
	}
}

// TestRelTypeAt covers resolving a relationship's type name by CSR position.
func TestRelTypeAt(t *testing.T) {
	s := buildGraph(t)
	pos := knowsPos(t, s, 0, 1)
	if ty, ok := s.RelTypeAt(pos); !ok || ty != "KNOWS" {
		t.Fatalf("RelTypeAt(%d) = %q,%v, want KNOWS", pos, ty, ok)
	}
}

// TestMatchedTraversalAccessors covers the pre-resolved-matcher walk
// accessors: the iterator, the two neighbor-append forms, the
// relationship-append form, the between-count, and the position seek.
func TestMatchedTraversalAccessors(t *testing.T) {
	s := buildGraph(t)
	knows := s.CompileRelMatcher([]string{"KNOWS"})
	all := s.CompileRelMatcher(nil)

	// RelationshipsMatched: alice's single KNOWS to bob.
	var rn []chickpeas.NodeID
	var rp []uint32
	for n, p := range s.RelationshipsMatched(0, chickpeas.Outgoing, knows) {
		rn = append(rn, n)
		rp = append(rp, p)
	}
	if !slices.Equal(rn, []chickpeas.NodeID{1}) || len(rp) != 1 {
		t.Fatalf("RelationshipsMatched = %v/%v", rn, rp)
	}

	// AppendNeighborsMatched: KNOWS -> [bob], all-types -> both neighbors.
	if nb := s.AppendNeighborsMatched(nil, 0, chickpeas.Outgoing, knows); !slices.Equal(nb, []chickpeas.NodeID{1}) {
		t.Fatalf("AppendNeighborsMatched KNOWS = %v", nb)
	}
	if nb := s.AppendNeighborsMatched(nil, 0, chickpeas.Outgoing, all); len(nb) != 2 {
		t.Fatalf("AppendNeighborsMatched all = %v, want 2", nb)
	}

	// AppendNeighborsByType: KNOWS -> [bob], empty types -> both.
	if bt := s.AppendNeighborsByType(nil, 0, chickpeas.Outgoing, []string{"KNOWS"}); !slices.Equal(bt, []chickpeas.NodeID{1}) {
		t.Fatalf("AppendNeighborsByType KNOWS = %v", bt)
	}
	if bt := s.AppendNeighborsByType(nil, 0, chickpeas.Outgoing, nil); len(bt) != 2 {
		t.Fatalf("AppendNeighborsByType empty = %v, want 2", bt)
	}

	// AppendRelationshipsMatched: parallel neighbor/position fill.
	ns, ps := s.AppendRelationshipsMatched(nil, nil, 0, chickpeas.Outgoing, knows)
	if !slices.Equal(ns, []chickpeas.NodeID{1}) || len(ps) != 1 {
		t.Fatalf("AppendRelationshipsMatched = %v/%v", ns, ps)
	}

	// CountNeighborsMatched: alice->bob is one KNOWS, alice->carol is none.
	if c := s.CountNeighborsMatched(0, 1, chickpeas.Outgoing, knows); c != 1 {
		t.Fatalf("count alice->bob KNOWS = %d, want 1", c)
	}
	if c := s.CountNeighborsMatched(0, 2, chickpeas.Outgoing, knows); c != 0 {
		t.Fatalf("count alice->carol KNOWS = %d, want 0", c)
	}

	// AppendRelsBetween: one position for alice-bob KNOWS, none alice-carol.
	if rb := s.AppendRelsBetween(nil, 0, 1, chickpeas.Outgoing, knows); len(rb) != 1 {
		t.Fatalf("AppendRelsBetween alice-bob = %v, want 1", rb)
	}
	if rb := s.AppendRelsBetween(nil, 0, 2, chickpeas.Outgoing, knows); len(rb) != 0 {
		t.Fatalf("AppendRelsBetween alice-carol = %v, want 0", rb)
	}
}

// TestCapabilityResolvers covers the single-type guards on the chain-collapse
// and functionality capability resolvers.
func TestCapabilityResolvers(t *testing.T) {
	s := buildGraph(t)

	// FunctionalVia rejects a multi-type request outright; a single type
	// whose every source carries at most one such rel is functional
	// (LIVES_IN: only alice, exactly one).
	if s.FunctionalVia([]string{"KNOWS", "LIVES_IN"}, chickpeas.Outgoing) {
		t.Fatal("multi-type FunctionalVia must be false")
	}
	if !s.FunctionalVia([]string{"LIVES_IN"}, chickpeas.Outgoing) {
		t.Fatal("LIVES_IN is functional outgoing (one per source)")
	}

	// ChainRootsVia rejects a multi-type request (nil, false); a single-type
	// request resolves through the engine, and a false result must carry a
	// nil RootsVia.
	if roots, ok := s.ChainRootsVia([]string{"A", "B"}, chickpeas.Outgoing, []string{"Person"}); ok || roots != nil {
		t.Fatalf("multi-type ChainRootsVia = %v,%v, want nil,false", roots, ok)
	}
	if roots, ok := s.ChainRootsVia([]string{"KNOWS"}, chickpeas.Outgoing, []string{"Person"}); !ok && roots != nil {
		t.Fatal("a false ChainRootsVia must return a nil RootsVia")
	}
}
