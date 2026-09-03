// Pre-resolved pattern matchers (port of the Rust NativeNodeMatcher /
// NativeRelMatcher): a node pattern's label and inline-property names are
// resolved to snapshot handles once per operator, so the per-candidate
// accept test is a bitmap contains plus a direct column read instead of
// interner lookups.
package graph

import (
	"sync"

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

// labelTest is one compiled label membership check: a dense label probes
// a word bitmap (one load and mask), the rest the compressed node set. An
// empty test (unknown label) rejects every candidate. A set-backed test
// counts its probes and self-densifies past adaptiveDenseProbes: probe
// volume, not label density, is what amortizes the id-space-proportional
// bitmap, and the engine caches it per label so one hot execution pays
// the build for every later compile. Matchers are compiled per stage per
// execution and evaluated single-threaded, so the plain counter is safe.
type labelTest struct {
	bits   []uint64
	set    *nodeset.Set
	label  string
	probes uint32
}

// adaptiveDenseProbes is the set-probe count past which a label test
// requests the floor-free bitmap (LabelDenseForced); heavy levels probe
// millions of times, so the ~2 ms build repays within one execution.
const adaptiveDenseProbes = 1 << 16

func (t *labelTest) contains(g *chickpeas.Snapshot, id uint32) bool {
	if t.bits != nil {
		w := int(id) >> 6
		return w < len(t.bits) && t.bits[w]>>(id&63)&1 == 1
	}
	if t.set == nil {
		return false
	}
	t.probes++
	if t.probes >= adaptiveDenseProbes {
		if t.bits = g.LabelDenseForced(t.label); t.bits != nil {
			w := int(id) >> 6
			return w < len(t.bits) && t.bits[w]>>(id&63)&1 == 1
		}
	}
	return t.set.Contains(id)
}

// NodeMatcher is a node pattern's labels and inline properties pre-resolved
// to snapshot handles.
type NodeMatcher struct {
	labels []labelTest
	props  []propPred
}

// CompileNodeMatcher resolves each label to its membership test (the
// engine's dense word bitmap when the label has one, the node set
// otherwise) and each inline key to its column once, so the per-candidate
// NodeMatcherAccepts avoids re-interning constant names.
func (s *SnapshotGraph) CompileNodeMatcher(labels []string, props []PropSpec) *NodeMatcher {
	m := &NodeMatcher{labels: make([]labelTest, len(labels)), props: make([]propPred, len(props))}
	for i, l := range labels {
		if set, ok := s.g.NodesWithLabel(l); ok {
			m.labels[i] = labelTest{bits: s.g.LabelDense(l), set: set, label: l}
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
	for i := range m.labels {
		if !m.labels[i].contains(s.g, uint32(node)) {
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

// FilterMatchedTail compacts ids[start:] in place to the matcher-accepted
// entries and returns the shortened slice; rels, when non-nil, is a
// parallel array compacted identically from relStart. Result-identical to
// testing each candidate with NodeMatcherAccepts -- the batch form exists
// so a whole expansion tail filters through one call, with the dominant
// matcher shape (one dense label, no inline properties) reduced to a
// hoisted word-bitmap loop: no per-candidate dispatch at all. Set-backed
// labels keep the per-candidate test, whose adaptive densification then
// upgrades later batches.
func (s *SnapshotGraph) FilterMatchedTail(m *NodeMatcher, ids []chickpeas.NodeID, start int, rels []uint32, relStart int) ([]chickpeas.NodeID, []uint32) {
	if len(m.props) == 0 && len(m.labels) == 1 && m.labels[0].bits != nil {
		bits := m.labels[0].bits
		w := start
		for i, id := range ids[start:] {
			wi := int(id) >> 6
			if wi < len(bits) && bits[wi]>>(uint32(id)&63)&1 == 1 {
				ids[w] = id
				if rels != nil {
					rels[relStart+(w-start)] = rels[relStart+i]
				}
				w++
			}
		}
		if rels != nil {
			rels = rels[:relStart+(w-start)]
		}
		return ids[:w], rels
	}
	w := start
	for i, id := range ids[start:] {
		if s.NodeMatcherAccepts(m, id) {
			ids[w] = id
			if rels != nil {
				rels[relStart+(w-start)] = rels[relStart+i]
			}
			w++
		}
	}
	if rels != nil {
		rels = rels[:relStart+(w-start)]
	}
	return ids[:w], rels
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

// Shareable reports whether the compiled matcher is safe to reuse across
// concurrent executions: a labelTest backed by a dense bitmap (or by
// nothing -- an unknown label rejects immutably) never mutates, while a
// set-backed test carries the per-execution probe counter and
// self-densifying swap that make it single-run only. Property predicates
// are immutable after compile in every form. The snapshot's label-bitmap
// cache self-warms (one hot execution densifies for every later
// compile), so warm-path matchers converge to shareable.
func (m *NodeMatcher) Shareable() bool {
	for i := range m.labels {
		t := &m.labels[i]
		if t.bits == nil && t.set != nil {
			return false
		}
	}
	return true
}

// matcherMemoCap bounds the memo; past it, compiles stay fresh. The
// corpus census measured 69 distinct node keys and 37 rel keys across
// 89 queries, so the cap is a runaway backstop, not a working limit.
const matcherMemoCap = 4096

// MatcherMemo carries compiled matchers across executions -- matcher
// compilation re-resolved labels, columns, and interned values per stage
// per run. One memo serves ONE snapshot (it adopts the first it sees and
// bypasses for any other); node matchers are stored only when Shareable.
// Keys encode the resolved VALUES (lifted parameters bake values into
// predicates at compile), so literal variants sharing a template plan
// never share a mismatched matcher.
type MatcherMemo struct {
	mu   sync.Mutex
	snap *chickpeas.Snapshot
	node map[string]*NodeMatcher
	rel  map[string]*RelMatcher
	// scratch backs key construction so a HIT allocates nothing: the
	// map lookup uses the compiler's non-allocating map[string(bytes)]
	// form, and only a store materializes the key string.
	scratch []byte
}

// adopt binds the memo to its first snapshot; reports whether s is owned.
func (mm *MatcherMemo) adopt(s *SnapshotGraph) bool {
	if mm.snap == nil {
		mm.snap = s.g
		mm.node = map[string]*NodeMatcher{}
		mm.rel = map[string]*RelMatcher{}
	}
	return mm.snap == s.g
}

// asSnapshot narrows the graph interface; only snapshot graphs memoize.
func asSnapshot(g Graph) (*SnapshotGraph, bool) {
	s, ok := g.(*SnapshotGraph)
	return s, ok
}

// appendNodeKey encodes labels and resolved property values into b.
func appendNodeKey(b []byte, labels []string, props []PropSpec) []byte {
	for _, l := range labels {
		b = append(b, l...)
		b = append(b, 0)
	}
	b = append(b, 1)
	for _, p := range props {
		b = append(b, p.Key...)
		b = append(b, 0)
		b = value.AppendKey(b, p.Val)
		b = append(b, 0)
	}
	return b
}

// CompileNodeMatcher is the memoized form; a nil memo, a foreign
// snapshot, an over-cap memo, or a non-shareable result all fall back to
// a fresh compile.
func (mm *MatcherMemo) CompileNodeMatcher(g Graph, labels []string, props []PropSpec) *NodeMatcher {
	s, isSnap := asSnapshot(g)
	if mm == nil || !isSnap {
		return g.CompileNodeMatcher(labels, props)
	}
	mm.mu.Lock()
	if !mm.adopt(s) {
		mm.mu.Unlock()
		return s.CompileNodeMatcher(labels, props)
	}
	mm.scratch = appendNodeKey(mm.scratch[:0], labels, props)
	if m, ok := mm.node[string(mm.scratch)]; ok {
		mm.mu.Unlock()
		return m
	}
	mm.mu.Unlock()
	m := s.CompileNodeMatcher(labels, props)
	if m.Shareable() {
		mm.mu.Lock()
		if len(mm.node) < matcherMemoCap {
			mm.node[string(appendNodeKey(mm.scratch[:0], labels, props))] = m
		}
		mm.mu.Unlock()
	}
	return m
}

// CompileRelMatcher is the memoized form; RelMatcher is immutable, so
// every result stores.
func (mm *MatcherMemo) CompileRelMatcher(g Graph, types []string) *RelMatcher {
	s, isSnap := asSnapshot(g)
	if mm == nil || !isSnap {
		return g.CompileRelMatcher(types)
	}
	mm.mu.Lock()
	if !mm.adopt(s) {
		mm.mu.Unlock()
		return s.CompileRelMatcher(types)
	}
	b := mm.scratch[:0]
	for _, t := range types {
		b = append(b, t...)
		b = append(b, 0)
	}
	mm.scratch = b
	if m, ok := mm.rel[string(b)]; ok {
		mm.mu.Unlock()
		return m
	}
	key := string(b)
	mm.mu.Unlock()
	m := s.CompileRelMatcher(types)
	mm.mu.Lock()
	if len(mm.rel) < matcherMemoCap {
		mm.rel[key] = m
	}
	mm.mu.Unlock()
	return m
}
