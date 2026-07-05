// Builder pre-finalization read probes: staged counts, property and label
// reads, value-scan and neighbor projections. All are O(pairs)/O(rels)
// scans over the staging state, never interning (reads must not grow the
// atom table).

package chickpeas

// NodeCount is the number of distinct nodes staged so far.
func (b *Builder) NodeCount() int {
	return int(b.knownNodes.GetCardinality())
}

// RelCount is the number of live relationships staged so far (tombstoned
// rels and pending detach-delete cascades excluded).
func (b *Builder) RelCount() int {
	if !b.hasRemovals() {
		return len(b.rels)
	}
	count := 0
	for idx := range b.rels {
		if !b.relRemoved(idx) {
			count++
		}
	}
	return count
}

// Prop reads node's staged value for key -- an O(pairs) pre-finalization
// probe that never interns (a string value never interned can't be staged
// anywhere). Returns the FIRST staged write, matching the Rust builder.
func (b *Builder) Prop(node NodeID, key string) (Value, bool) {
	k, ok := b.interner.Get(key)
	if !ok {
		return Value{}, false
	}
	for _, p := range b.nodeColStr[k] {
		if p.id == node {
			return StrValue(p.val), true
		}
	}
	for _, p := range b.nodeColI64[k] {
		if p.id == node {
			return I64Value(p.val), true
		}
	}
	for _, p := range b.nodeColF64[k] {
		if p.id == node {
			return F64Value(p.val), true
		}
	}
	for _, p := range b.nodeColBool[k] {
		if p.id == node {
			return BoolValue(p.val), true
		}
	}
	return Value{}, false
}

// ResolveString resolves an interner atom back to its string (for reading
// staged Prop values); ok is false when out of range.
func (b *Builder) ResolveString(id uint32) (string, bool) {
	return b.interner.Resolve(id)
}

// NodesWithProperty lists the label-carrying nodes whose staged property
// key equals value, in staging order -- an O(pairs) pre-finalization scan.
// A string value never interned in this builder can't be on any node, so
// the probe returns empty WITHOUT interning it (reads must not grow the
// atom table).
func (b *Builder) NodesWithProperty(label, key string, value any) []NodeID {
	labelAtom, ok := b.interner.Get(label)
	if !ok {
		return nil
	}
	k, ok := b.interner.Get(key)
	if !ok {
		return nil
	}
	var want Value
	switch v := value.(type) {
	case Value:
		want = v
	case string:
		atom, interned := b.interner.Get(v)
		if !interned {
			return nil
		}
		want = StrValue(atom)
	default:
		staged, err := b.stageValue(value) // numeric/bool: never interns
		if err != nil {
			return nil
		}
		want = staged
	}
	hasLabel := func(node NodeID) bool {
		if int(node) >= len(b.nodeLabels) {
			return false
		}
		for _, l := range b.nodeLabels[node] {
			if l.ID() == labelAtom {
				return true
			}
		}
		return false
	}
	var nodes []NodeID
	// Scan only the column whose type matches the probe value.
	switch want.Kind() {
	case KindI64:
		target, _ := want.I64()
		for _, p := range b.nodeColI64[k] {
			if p.val == target && hasLabel(p.id) {
				nodes = append(nodes, p.id)
			}
		}
	case KindF64:
		for _, p := range b.nodeColF64[k] {
			if F64Value(p.val) == want && hasLabel(p.id) {
				nodes = append(nodes, p.id)
			}
		}
	case KindBool:
		target, _ := want.Bool()
		for _, p := range b.nodeColBool[k] {
			if p.val == target && hasLabel(p.id) {
				nodes = append(nodes, p.id)
			}
		}
	case KindStr:
		target, _ := want.StrID()
		for _, p := range b.nodeColStr[k] {
			if p.val == target && hasLabel(p.id) {
				nodes = append(nodes, p.id)
			}
		}
	}
	return nodes
}

// NodeLabels lists node's staged labels in insertion order.
func (b *Builder) NodeLabels(node NodeID) []string {
	if int(node) >= len(b.nodeLabels) {
		return nil
	}
	out := make([]string, 0, len(b.nodeLabels[node]))
	for _, l := range b.nodeLabels[node] {
		if s, ok := b.interner.Resolve(l.ID()); ok {
			out = append(out, s)
		}
	}
	return out
}

// NeighborIDs lists node's staged neighbors in direction (outgoing then
// incoming for Both), skipping removed rels -- an O(rels) pre-finalization
// scan.
func (b *Builder) NeighborIDs(node NodeID, dir Direction) []NodeID {
	var out []NodeID
	if dir == Outgoing || dir == Both {
		for idx, r := range b.rels {
			if r[0] == node && !b.relRemoved(idx) {
				out = append(out, r[1])
			}
		}
	}
	if dir == Incoming || dir == Both {
		for idx, r := range b.rels {
			if r[1] == node && !b.relRemoved(idx) {
				out = append(out, r[0])
			}
		}
	}
	return out
}
