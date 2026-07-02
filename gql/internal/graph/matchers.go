// Pre-resolved pattern matchers (port of the Rust NativeNodeMatcher /
// NativeRelMatcher): a node pattern's label and inline-property names are
// resolved to snapshot handles once per operator, so the per-candidate
// accept test is a bitmap contains plus a direct column read instead of
// interner lookups.
package graph

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// RelMatcher is an expand's relationship types pre-resolved to interned
// ids. Wraps the engine's RelMatch, which has the exact semantics the seam
// needs: no types matches all, unknown names drop, all-unknown matches
// nothing.
type RelMatcher struct {
	m chickpeas.RelMatch
}

// propPredKind discriminates one inline-property predicate.
type propPredKind uint8

const (
	// predNoColumn: the key has no column, so every node reads absent for
	// it -- matching only a Null value.
	predNoColumn propPredKind = iota
	// predStrEq: a string equality with the comparison text pre-interned
	// to its atom once; the per-candidate check is an id compare. atomOK
	// false means the text is absent from the interner, so nothing can
	// equal it.
	predStrEq
	// predGeneric: read the stored value and compare via value.Equal
	// (numeric coercion, bool, Null-absent), byte-identical to NodePropEq.
	predGeneric
)

// propPred is one inline {key: value} predicate with its key pre-resolved
// to a column reader.
type propPred struct {
	kind     propPredKind
	read     func(pos uint32) (value.Value, bool)
	atom     uint32
	atomOK   bool
	readAtom func(pos uint32) (uint32, bool)
	expected value.Value
}

// NodeMatcher is a node pattern's labels and inline properties pre-resolved
// to snapshot handles. A nil labels entry is a label unknown to the graph,
// which rejects every candidate.
type NodeMatcher struct {
	labels []*nodeset.Set
	props  []propPred
}

// CompileNodeMatcher resolves each label to its node set and each inline
// key to its column once, so the per-candidate NodeMatcherAccepts avoids
// re-interning constant names.
func (s *SnapshotGraph) CompileNodeMatcher(labels []string, props []PropSpec) *NodeMatcher {
	m := &NodeMatcher{labels: make([]*nodeset.Set, len(labels)), props: make([]propPred, len(props))}
	for i, l := range labels {
		if set, ok := s.g.NodesWithLabel(l); ok {
			m.labels[i] = set
		}
	}
	for i, p := range props {
		col, ok := s.g.ColIndexed(p.Key)
		if !ok {
			m.props[i] = propPred{kind: predNoColumn, expected: p.Val}
			continue
		}
		if txt, isStr := p.Val.AsStr(); isStr {
			pred := propPred{kind: predStrEq, readAtom: strColReader(col)}
			if v, ok := s.g.ValueFromString(txt); ok {
				pred.atom, pred.atomOK = v.StrID()
			}
			m.props[i] = pred
			continue
		}
		m.props[i] = propPred{kind: predGeneric, read: s.colReader(col), expected: p.Val}
	}
	return m
}

// NodeMatcherAccepts reports whether node carries every label and matches
// every inline property of the compiled matcher.
func (s *SnapshotGraph) NodeMatcherAccepts(m *NodeMatcher, node chickpeas.NodeID) bool {
	for _, set := range m.labels {
		if set == nil || !set.Contains(uint32(node)) {
			return false
		}
	}
	for i := range m.props {
		p := &m.props[i]
		switch p.kind {
		case predNoColumn:
			if !p.expected.IsNull() {
				return false
			}
		case predStrEq:
			id, ok := p.readAtom(uint32(node))
			if !ok || !p.atomOK || id != p.atom {
				return false
			}
		case predGeneric:
			v, ok := p.read(uint32(node))
			if !ok {
				if !p.expected.IsNull() {
					return false
				}
			} else if !value.Equal(v, p.expected) {
				return false
			}
		}
	}
	return true
}

// CompileRelMatcher pre-resolves relationship-type names via the engine's
// Match (empty = all, unknown names drop, all-unknown = none).
func (s *SnapshotGraph) CompileRelMatcher(types []string) *RelMatcher {
	return &RelMatcher{m: s.g.Match(types...)}
}

// strColReader reads the stored atom id at a position, folding the dense
// string column's atom-0-means-missing convention to absent (the raw
// StrCol.ID does not).
func strColReader(col chickpeas.Col) func(pos uint32) (uint32, bool) {
	r := col.Str()
	return func(pos uint32) (uint32, bool) {
		id, ok := r.ID(pos)
		if !ok || id == 0 {
			return 0, false
		}
		return id, true
	}
}

// colReader narrows a column once by dtype and returns a positional runtime
// -value reader (strings resolved through the interner, empty folding to
// absent per the engine's Prop convention).
func (s *SnapshotGraph) colReader(col chickpeas.Col) func(pos uint32) (value.Value, bool) {
	switch col.Dtype() {
	case chickpeas.DtypeI64:
		r := col.I64()
		return func(pos uint32) (value.Value, bool) {
			v, ok := r.Get(pos)
			if !ok {
				return value.Value{}, false
			}
			return value.Int(v), true
		}
	case chickpeas.DtypeF64:
		r := col.F64()
		return func(pos uint32) (value.Value, bool) {
			v, ok := r.Get(pos)
			if !ok {
				return value.Value{}, false
			}
			return value.Float(v), true
		}
	case chickpeas.DtypeBool:
		r := col.Bool()
		return func(pos uint32) (value.Value, bool) {
			v, ok := r.Get(pos)
			if !ok {
				return value.Value{}, false
			}
			return value.Bool(v), true
		}
	default:
		readAtom := strColReader(col)
		return func(pos uint32) (value.Value, bool) {
			id, ok := readAtom(pos)
			if !ok {
				return value.Value{}, false
			}
			txt, found := s.g.ResolveString(id)
			if !found || txt == "" {
				return value.Value{}, false
			}
			return value.Str(txt), true
		}
	}
}
