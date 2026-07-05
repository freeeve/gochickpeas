package chickpeas_test

import (
	"fmt"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// The fuzz oracle: a naive map/slice reference model of builder staging with
// removals. Node ids, keys, labels, and rel types draw from small universes
// so operations collide often (duplicate writes, parallel rels, re-removals,
// resurrections).
const (
	oracleMaxNode = 8
	oracleKeys    = 4
)

type oracleNode struct {
	labels map[string]bool
	props  map[string]any
}

type oracleRel struct {
	u, v    chickpeas.NodeID
	typ     string
	props   map[string]any
	removed bool
}

type oracleModel struct {
	nodes map[chickpeas.NodeID]*oracleNode
	rels  []*oracleRel
	// thawBase is the rel count at the mid-run thaw: model list indexes below
	// it no longer correspond to builder rel indexes (the thaw restages live
	// rels in a linear extension of the CSR orders), so index-addressed ops
	// on them are skipped after the thaw. Per-node projections and
	// (u, v, type) first-match addressing survive the reorder.
	thawBase int
}

func (m *oracleModel) node(id chickpeas.NodeID) *oracleNode {
	n, ok := m.nodes[id]
	if !ok {
		n = &oracleNode{labels: map[string]bool{}, props: map[string]any{}}
		m.nodes[id] = n
	}
	return n
}

func (m *oracleModel) removeNode(id chickpeas.NodeID) {
	if _, ok := m.nodes[id]; !ok {
		return
	}
	delete(m.nodes, id)
	// Watermark semantics: rels staged so far die; rels added later (which
	// resurrect the node) will be fresh entries.
	for _, r := range m.rels {
		if r.u == id || r.v == id {
			r.removed = true
		}
	}
}

// firstLiveRel is the model mirror of the builder's (u, v, type) first-match
// addressing; group order is preserved across thaw, so the first live match
// coincides in both.
func (m *oracleModel) firstLiveRel(u, v chickpeas.NodeID, typ string) *oracleRel {
	for _, r := range m.rels {
		if !r.removed && r.u == u && r.v == v && r.typ == typ {
			return r
		}
	}
	return nil
}

// opReader decodes the fuzz byte stream into bounded operands.
type opReader struct {
	data []byte
	pos  int
}

func (r *opReader) byte() (byte, bool) {
	if r.pos >= len(r.data) {
		return 0, false
	}
	b := r.data[r.pos]
	r.pos++
	return b, true
}

func oracleLabel(b byte) string { return fmt.Sprintf("L%d", b%2) }
func oracleType(b byte) string  { return fmt.Sprintf("T%d", b%2) }
func oracleKey(b byte) string   { return fmt.Sprintf("k%d", b%oracleKeys) }

// oracleValue decodes a property value; the kind is the KEY byte, so each
// key stays a single value type across the whole run (the snapshot format
// stores one column -- one dtype -- per key; a key mixing types across
// different nodes is resolved by Finalize's column loop order, which a naive
// oracle cannot model). Payloads are mostly non-zero so stale values cannot
// hide behind the dense zero-fill tolerance.
func oracleValue(kind, payload byte) any {
	switch kind % 4 {
	case 0:
		return int64(payload%7) + 1
	case 1:
		return float64(payload%5) + 0.5
	case 2:
		return payload%2 == 0
	default:
		return fmt.Sprintf("s%d", payload%3)
	}
}

// applyOp decodes and applies one operation to both the builder and the
// model, keeping them in lockstep.
func applyOp(t *testing.T, b *chickpeas.Builder, m *oracleModel, r *opReader) bool {
	t.Helper()
	op, ok := r.byte()
	if !ok {
		return false
	}
	arg := func() (byte, bool) { return r.byte() }
	switch op % 9 {
	case 0: // AddNodeWithID(id, label)
		a1, ok1 := arg()
		a2, ok2 := arg()
		if !ok1 || !ok2 {
			return false
		}
		id, label := chickpeas.NodeID(a1%oracleMaxNode), oracleLabel(a2)
		if _, err := b.AddNodeWithID(id, label); err != nil {
			t.Fatal(err)
		}
		m.node(id).labels[label] = true
	case 1: // AddRel(u, v, T)
		a1, ok1 := arg()
		a2, ok2 := arg()
		a3, ok3 := arg()
		if !ok1 || !ok2 || !ok3 {
			return false
		}
		u, v, typ := chickpeas.NodeID(a1%oracleMaxNode), chickpeas.NodeID(a2%oracleMaxNode), oracleType(a3)
		idx, err := b.AddRel(u, v, typ)
		if err != nil {
			t.Fatal(err)
		}
		if idx != len(m.rels) {
			t.Fatalf("rel index drift: builder %d, model %d", idx, len(m.rels))
		}
		m.node(u)
		m.node(v)
		m.rels = append(m.rels, &oracleRel{u: u, v: v, typ: typ, props: map[string]any{}})
	case 2: // UpdateProp(node, key, val)
		a1, ok1 := arg()
		a2, ok2 := arg()
		a3, ok3 := arg()
		if !ok1 || !ok2 || !ok3 {
			return false
		}
		id, key, val := chickpeas.NodeID(a1%oracleMaxNode), oracleKey(a2), oracleValue(a2, a3)
		if err := b.UpdateProp(id, key, val); err != nil {
			t.Fatal(err)
		}
		m.node(id).props[key] = val
	case 3: // RemoveProp(node, key)
		a1, ok1 := arg()
		a2, ok2 := arg()
		if !ok1 || !ok2 {
			return false
		}
		id, key := chickpeas.NodeID(a1%oracleMaxNode), oracleKey(a2)
		b.RemoveProp(id, key)
		if n, ok := m.nodes[id]; ok {
			delete(n.props, key)
		}
	case 4: // SetRelProp(u, v, T, key, val) -- first-match addressing
		a1, ok1 := arg()
		a2, ok2 := arg()
		a3, ok3 := arg()
		a4, ok4 := arg()
		a5, ok5 := arg()
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return false
		}
		u, v, typ := chickpeas.NodeID(a1%oracleMaxNode), chickpeas.NodeID(a2%oracleMaxNode), oracleType(a3)
		key, val := oracleKey(a4), oracleValue(a4, a5)
		err := b.SetRelProp(u, v, typ, key, val)
		if rel := m.firstLiveRel(u, v, typ); rel != nil {
			if err != nil {
				t.Fatalf("SetRelProp on live rel: %v", err)
			}
			rel.props[key] = val
		} else if err == nil {
			t.Fatal("SetRelProp succeeded on a rel the model says is dead or absent")
		}
	case 5: // RemoveRelProp(u, v, T, key)
		a1, ok1 := arg()
		a2, ok2 := arg()
		a3, ok3 := arg()
		a4, ok4 := arg()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return false
		}
		u, v, typ := chickpeas.NodeID(a1%oracleMaxNode), chickpeas.NodeID(a2%oracleMaxNode), oracleType(a3)
		key := oracleKey(a4)
		removed, err := b.RemoveRelProp(u, v, typ, key)
		if rel := m.firstLiveRel(u, v, typ); rel != nil {
			if err != nil {
				t.Fatalf("RemoveRelProp on live rel: %v", err)
			}
			// One-directional check: a key the model holds must report
			// removed. The converse doesn't hold across the mid-run thaw --
			// a dense column materializes staged zero pairs at positions the
			// model treats as absent (the documented dense lossiness), and
			// removing one is a real removal the model can't see.
			if _, had := rel.props[key]; had && !removed {
				t.Fatalf("RemoveRelProp removed=false, model had key %q", key)
			}
			delete(rel.props, key)
		} else if err == nil {
			t.Fatal("RemoveRelProp succeeded on a dead or absent rel")
		}
	case 6: // RemoveRel(idx)
		a1, ok1 := arg()
		if !ok1 {
			return false
		}
		if len(m.rels) == 0 {
			return true
		}
		idx := int(a1) % len(m.rels)
		if idx < m.thawBase {
			return true // stale index across the thaw reorder
		}
		err := b.RemoveRel(idx)
		if rel := m.rels[idx]; !rel.removed {
			if err != nil {
				t.Fatalf("RemoveRel on live rel %d: %v", idx, err)
			}
			rel.removed = true
		} else if err == nil {
			t.Fatalf("RemoveRel succeeded twice on rel %d", idx)
		}
	case 7: // RemoveNode(id)
		a1, ok1 := arg()
		if !ok1 {
			return false
		}
		id := chickpeas.NodeID(a1 % oracleMaxNode)
		_, known := m.nodes[id]
		if got := b.RemoveNode(id); got != known {
			t.Fatalf("RemoveNode(%d) = %v, model knows node: %v", id, got, known)
		}
		m.removeNode(id)
	case 8: // SetRelPropAt(idx, key, val)
		a1, ok1 := arg()
		a2, ok2 := arg()
		a3, ok3 := arg()
		if !ok1 || !ok2 || !ok3 {
			return false
		}
		if len(m.rels) == 0 {
			return true
		}
		idx := int(a1) % len(m.rels)
		if idx < m.thawBase {
			return true
		}
		key, val := oracleKey(a2), oracleValue(a2, a3)
		err := b.SetRelPropAt(idx, key, val)
		if rel := m.rels[idx]; !rel.removed {
			if err != nil {
				t.Fatalf("SetRelPropAt on live rel %d: %v", idx, err)
			}
			rel.props[key] = val
		} else if err == nil {
			t.Fatalf("SetRelPropAt succeeded on removed rel %d", idx)
		}
	}
	return true
}

// propMatches compares a snapshot property read against the model's
// expectation. An absent expectation tolerates the dense layout's
// present-as-zero (0, +0.0, false, "") -- the documented dense lossiness --
// but any other surviving value is a resurrection bug.
func propMatches(p chickpeas.Prop, expected any, ok bool) error {
	v, present := p.Value()
	if !ok {
		if !present {
			return nil
		}
		if slices.Contains([]chickpeas.Value{
			chickpeas.I64Value(0), chickpeas.F64Value(0), chickpeas.BoolValue(false), chickpeas.StrValue(0),
		}, v) {
			return nil
		}
		return fmt.Errorf("expected absent, got %v", v)
	}
	switch e := expected.(type) {
	case int64:
		if got, isI64 := p.I64(); !isI64 || got != e {
			return fmt.Errorf("want i64 %d, got %v (present %v)", e, v, present)
		}
	case float64:
		if got, isF64 := p.F64(); !isF64 || got != e {
			return fmt.Errorf("want f64 %g, got %v (present %v)", e, v, present)
		}
	case bool:
		if got, isBool := p.Bool(); !isBool || got != e {
			return fmt.Errorf("want bool %v, got %v (present %v)", e, v, present)
		}
	case string:
		if got, isStr := p.Str(); !isStr || got != e {
			return fmt.Errorf("want str %q, got %v (present %v)", e, v, present)
		}
	}
	return nil
}

// verifyOracle compares the finalized snapshot against the model: counts,
// per-node labels and properties, per-node rel projections in both
// directions (order within a node's rels is staging order, which every
// removal and thaw path must preserve), and per-rel properties.
func verifyOracle(t *testing.T, g *chickpeas.Snapshot, m *oracleModel) {
	t.Helper()
	if int(g.NodeCount()) != len(m.nodes) {
		t.Fatalf("node count: snapshot %d, model %d", g.NodeCount(), len(m.nodes))
	}
	live := 0
	for _, r := range m.rels {
		if !r.removed {
			live++
		}
	}
	if int(g.RelCount()) != live {
		t.Fatalf("rel count: snapshot %d, model %d", g.RelCount(), live)
	}
	allKeys := make([]string, oracleKeys)
	for i := range allKeys {
		allKeys[i] = fmt.Sprintf("k%d", i)
	}
	for id, n := range m.nodes {
		for _, label := range []string{"L0", "L1"} {
			if g.HasLabel(id, label) != n.labels[label] {
				t.Fatalf("node %d label %s: snapshot %v, model %v",
					id, label, g.HasLabel(id, label), n.labels[label])
			}
		}
		for _, key := range allKeys {
			expected, ok := n.props[key]
			if err := propMatches(g.Prop(id, key), expected, ok); err != nil {
				t.Fatalf("node %d prop %s: %v", id, key, err)
			}
		}
		// Outgoing projection: the model's live rels from id, in list order,
		// must line up 1:1 with the snapshot's CSR order.
		var outRels []*oracleRel
		for _, r := range m.rels {
			if !r.removed && r.u == id {
				outRels = append(outRels, r)
			}
		}
		i := 0
		for ref := range g.Rels(id, chickpeas.Outgoing) {
			if i >= len(outRels) {
				t.Fatalf("node %d: snapshot has more outgoing rels than model", id)
			}
			r := outRels[i]
			typ, _ := g.ResolveString(ref.Type.ID())
			if ref.Neighbor != r.v || typ != r.typ {
				t.Fatalf("node %d out rel %d: snapshot (%d,%s), model (%d,%s)",
					id, i, ref.Neighbor, typ, r.v, r.typ)
			}
			for _, key := range allKeys {
				expected, ok := r.props[key]
				if err := propMatches(g.RelProp(ref.Pos, key), expected, ok); err != nil {
					t.Fatalf("node %d out rel %d prop %s: %v", id, i, key, err)
				}
			}
			i++
		}
		if i != len(outRels) {
			t.Fatalf("node %d: snapshot has %d outgoing rels, model %d", id, i, len(outRels))
		}
		// Incoming projection (endpoint + type only; props are covered by
		// the outgoing walk).
		var inRels []*oracleRel
		for _, r := range m.rels {
			if !r.removed && r.v == id {
				inRels = append(inRels, r)
			}
		}
		i = 0
		for ref := range g.Rels(id, chickpeas.Incoming) {
			if i >= len(inRels) {
				t.Fatalf("node %d: snapshot has more incoming rels than model", id)
			}
			typ, _ := g.ResolveString(ref.Type.ID())
			if ref.Neighbor != inRels[i].u || typ != inRels[i].typ {
				t.Fatalf("node %d in rel %d: snapshot (%d,%s), model (%d,%s)",
					id, i, ref.Neighbor, typ, inRels[i].u, inRels[i].typ)
			}
			i++
		}
		if i != len(inRels) {
			t.Fatalf("node %d: snapshot has %d incoming rels, model %d", id, i, len(inRels))
		}
	}
}

// thawModel mirrors NewBuilderFromSnapshot on the model: ghosts (no labels,
// no live rels, no column trace in g) are lost, ids a dense column zero-fills
// materialize as fresh known nodes (the documented inflation), dead rels
// compact away, and stale rel indexes are fenced off via thawBase.
func thawModel(m *oracleModel, g *chickpeas.Snapshot) {
	for id := chickpeas.NodeID(0); id < chickpeas.NodeID(g.CSRIDSpace()); id++ {
		if _, known := m.nodes[id]; known {
			continue
		}
		for i := range oracleKeys {
			if _, ok := g.Prop(id, fmt.Sprintf("k%d", i)).Value(); ok {
				m.node(id) // zero-filled position becomes a known node
				break
			}
		}
	}
	for id, n := range m.nodes {
		if len(n.labels) > 0 {
			continue
		}
		survives := false
		for _, r := range m.rels {
			if !r.removed && (r.u == id || r.v == id) {
				survives = true
				break
			}
		}
		for i := 0; !survives && i < oracleKeys; i++ {
			if _, ok := g.Prop(id, fmt.Sprintf("k%d", i)).Value(); ok {
				survives = true
			}
		}
		if !survives {
			delete(m.nodes, id)
		}
	}
	liveRels := m.rels[:0]
	for _, r := range m.rels {
		if !r.removed {
			liveRels = append(liveRels, r)
		}
	}
	m.rels = liveRels
	m.thawBase = len(m.rels)
}

// FuzzRemovalOracle drives random add/remove/set sequences against the
// map-based reference model, asserting post-Finalize snapshot equality --
// once on a single builder, and once with a Finalize + thaw round trip in
// the middle of the sequence (removal x thaw interactions).
func FuzzRemovalOracle(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 2, 3, 0, 1, 1, 2, 3, 6, 0, 7, 1, 1, 2, 3, 0})
	f.Add([]byte{1, 0, 1, 0, 4, 0, 1, 0, 1, 2, 3, 5, 0, 1, 0, 2, 8, 0, 1, 2, 3})
	f.Add([]byte{2, 0, 0, 0, 5, 2, 0, 0, 3, 5, 3, 0, 0, 7, 0, 2, 0, 1, 1, 6})
	f.Add([]byte{0, 3, 1, 1, 3, 4, 0, 6, 0, 7, 3, 1, 3, 4, 0, 1, 3, 4, 1, 8, 0, 2, 0, 9})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Single builder, ops end to end.
		b := chickpeas.NewBuilder(0, 0)
		m := &oracleModel{nodes: map[chickpeas.NodeID]*oracleNode{}}
		r := &opReader{data: data}
		for applyOp(t, b, m, r) {
		}
		verifyOracle(t, b.Finalize(), m)

		// Same ops with a Finalize + thaw at the midpoint.
		b = chickpeas.NewBuilder(0, 0)
		m = &oracleModel{nodes: map[chickpeas.NodeID]*oracleNode{}}
		r = &opReader{data: data[:len(data)/2]}
		for applyOp(t, b, m, r) {
		}
		mid := b.Finalize()
		verifyOracle(t, mid, m)
		thawModel(m, mid)
		b = chickpeas.NewBuilderFromSnapshot(mid)
		r = &opReader{data: data[len(data)/2:]}
		for applyOp(t, b, m, r) {
		}
		verifyOracle(t, b.Finalize(), m)
	})
}
