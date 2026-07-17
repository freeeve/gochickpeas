// Label-conditional degree statistics: the average number of relationships
// of one type, in one direction, per node carrying one label. The global
// AvgDegree averages a type's count over the type's OWN sources, which says
// nothing about how a particular label's nodes fan out over it -- a type
// whose rels overwhelmingly leave one label reads as a huge average for
// every label that merely touches it. A chain-cost estimator multiplying
// hop fan-outs needs the conditional form, or a low-cardinality anchor
// hides an exploding chain behind it.
package chickpeas

// labelDegreeEntry is one label's per-type incident-rel counts by
// direction, summed over every node carrying the label. Zero-degree
// members stay in the denominator: the statistic answers "expected fan-out
// of a node with this label", not "average over nodes having such a rel".
type labelDegreeEntry struct {
	out, in map[RelType]uint64
}

// labelDegreeStats returns (building once) the label's per-type degree
// counts. Same lazy-cache choreography as the other snapshot indexes:
// check under lock, build outside it, re-acquire and keep the first
// insert.
func (g *Snapshot) labelDegreeStats(l Label) *labelDegreeEntry {
	g.labelDegreeMu.Lock()
	if e, ok := g.labelDegrees[l]; ok {
		g.labelDegreeMu.Unlock()
		return e
	}
	g.labelDegreeMu.Unlock()
	e := &labelDegreeEntry{out: map[RelType]uint64{}, in: map[RelType]uint64{}}
	if set, ok := g.labelIndex[l]; ok {
		for id := range set.Iter() {
			u := int(id)
			if u+1 < len(g.outOffsets) {
				for i := g.outOffsets[u]; i < g.outOffsets[u+1]; i++ {
					e.out[g.outTypes[i]]++
				}
			}
			if u+1 < len(g.inOffsets) {
				for i := g.inOffsets[u]; i < g.inOffsets[u+1]; i++ {
					e.in[g.inTypes[i]]++
				}
			}
		}
	}
	g.labelDegreeMu.Lock()
	defer g.labelDegreeMu.Unlock()
	if prior, ok := g.labelDegrees[l]; ok {
		return prior
	}
	g.labelDegrees[l] = e
	return e
}

// AvgDegreeByLabel is AvgDegree conditioned on the node's label: the
// average number of relType relationships in dir per node carrying label,
// zero-degree members included. ok is false for an unknown or empty label
// (the caller falls back to the global statistic); a known label with no
// such rels answers (0, true) -- a real "this hop never fires" fact, not
// a missing statistic. Both averages the two directions' counts together.
func (g *Snapshot) AvgDegreeByLabel(label, relType string, dir Direction) (float64, bool) {
	l, ok := g.Label(label)
	if !ok {
		return 0, false
	}
	set, ok := g.labelIndex[l]
	if !ok || set.Len() == 0 {
		return 0, false
	}
	t, ok := g.RelType(relType)
	if !ok {
		return 0, true
	}
	e := g.labelDegreeStats(l)
	n := float64(set.Len())
	switch dir {
	case Outgoing:
		return float64(e.out[t]) / n, true
	case Incoming:
		return float64(e.in[t]) / n, true
	}
	return float64(e.out[t]+e.in[t]) / n, true
}
