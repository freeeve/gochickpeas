// Builder: the mutable staging area for constructing graphs. Collect nodes,
// rels, and properties, then Finalize into an immutable Snapshot. Node and
// rel ceilings are u32 (the CSR indexes rels by u32 position).
//
// The bulk-loader surfaces of the Rust builder (CSV/Parquet dedup
// machinery) are out of scope -- RCPG files are the interchange format.

package chickpeas

import (
	"errors"
	"fmt"

	"github.com/RoaringBitmap/roaring/v2"
)

// ErrCapacity reports exceeding the u32 node/rel ceiling.
var ErrCapacity = errors.New("capacity exceeded")

// ErrRelNotFound reports a rel-property set on a (u, v, type) that was
// never added.
var ErrRelNotFound = errors.New("relationship not found")

// ErrBadValue reports a property value of an unsupported type.
var ErrBadValue = errors.New("unsupported property value type")

type i64Pair struct {
	id  uint32
	val int64
}
type f64Pair struct {
	id  uint32
	val float64
}
type boolPair struct {
	id  uint32
	val bool
}
type strPair struct {
	id  uint32
	val uint32
}

// Builder stages a graph for finalization.
type Builder struct {
	degOut, degIn []uint32
	rels          [][2]NodeID
	nodeLabels    [][]Label
	relTypes      []RelType
	version       *string

	nodeColI64  map[PropertyKey][]i64Pair
	nodeColF64  map[PropertyKey][]f64Pair
	nodeColBool map[PropertyKey][]boolPair
	nodeColStr  map[PropertyKey][]strPair

	// Rel properties stage by rel index in rels; Finalize remaps them to
	// outgoing-CSR positions.
	relColI64  map[PropertyKey][]i64Pair
	relColF64  map[PropertyKey][]f64Pair
	relColBool map[PropertyKey][]boolPair
	relColStr  map[PropertyKey][]strPair

	interner   *Interner
	nextNodeID NodeID
	knownNodes *roaring.Bitmap

	// relIndex is the lazy (u, v, type) -> first rel index map behind
	// SetRelProp, built on first use and maintained by AddRel so setting
	// one property per rel is O(m + p), not O(m^2).
	relIndex map[[3]uint32]int
}

// NewBuilder returns a builder with capacity hints (0 = the 2^20 default);
// the builder auto-grows past them.
func NewBuilder(capNodes, capRels int) *Builder {
	const defaultCapacity = 1 << 20
	if capNodes <= 0 {
		capNodes = defaultCapacity
	}
	if capRels <= 0 {
		capRels = defaultCapacity
	}
	return &Builder{
		degOut:      make([]uint32, capNodes),
		degIn:       make([]uint32, capNodes),
		rels:        make([][2]NodeID, 0, capRels),
		nodeLabels:  make([][]Label, capNodes),
		relTypes:    make([]RelType, 0, capRels),
		nodeColI64:  map[PropertyKey][]i64Pair{},
		nodeColF64:  map[PropertyKey][]f64Pair{},
		nodeColBool: map[PropertyKey][]boolPair{},
		nodeColStr:  map[PropertyKey][]strPair{},
		relColI64:   map[PropertyKey][]i64Pair{},
		relColF64:   map[PropertyKey][]f64Pair{},
		relColBool:  map[PropertyKey][]boolPair{},
		relColStr:   map[PropertyKey][]strPair{},
		interner:    NewInterner(),
		knownNodes:  roaring.New(),
	}
}

// SetVersion sets the snapshot-level version string.
func (b *Builder) SetVersion(version string) {
	b.version = &version
}

// ensureCapacity grows the per-node arrays to cover id.
func (b *Builder) ensureCapacity(id NodeID) error {
	if int(id) < len(b.degOut) {
		return nil
	}
	const maxSize = int(^uint32(0))
	newSize := min((int(id)+1)*2, maxSize)
	if newSize <= int(id) {
		return fmt.Errorf("%w: maximum node limit (2^32 - 1)", ErrCapacity)
	}
	grow := func(s []uint32) []uint32 {
		out := make([]uint32, newSize)
		copy(out, s)
		return out
	}
	b.degOut = grow(b.degOut)
	b.degIn = grow(b.degIn)
	labels := make([][]Label, newSize)
	copy(labels, b.nodeLabels)
	b.nodeLabels = labels
	return nil
}

// AddNode adds a node with an auto-generated sequential id.
func (b *Builder) AddNode(labels ...string) (NodeID, error) {
	id := b.nextNodeID
	if id == ^uint32(0) {
		return 0, fmt.Errorf("%w: node id counter reached u32 max", ErrCapacity)
	}
	b.nextNodeID = id + 1
	return b.addNodeAt(id, labels)
}

// AddNodeWithID adds (or re-labels) the node with the given id; callers map
// their own identifiers onto the u32 space.
func (b *Builder) AddNodeWithID(id NodeID, labels ...string) (NodeID, error) {
	if id >= b.nextNodeID {
		b.nextNodeID = id + 1
	}
	return b.addNodeAt(id, labels)
}

func (b *Builder) addNodeAt(id NodeID, labels []string) (NodeID, error) {
	if err := b.ensureCapacity(id); err != nil {
		return 0, err
	}
	for _, l := range labels {
		b.nodeLabels[id] = append(b.nodeLabels[id], Label(b.interner.GetOrIntern(l)))
	}
	b.knownNodes.Add(id)
	return id, nil
}

// AddRel adds a relationship from u to v, returning its rel index (usable
// with SetRelPropAt). Endpoints are registered as known nodes.
func (b *Builder) AddRel(u, v NodeID, relType string) (int, error) {
	if err := b.ensureCapacity(max(u, v)); err != nil {
		return 0, err
	}
	if len(b.rels) >= int(^uint32(0)) {
		return 0, fmt.Errorf("%w: maximum rel limit (2^32 - 1)", ErrCapacity)
	}
	b.degOut[u]++
	b.degIn[v]++
	idx := len(b.rels)
	b.rels = append(b.rels, [2]NodeID{u, v})
	typeID := b.interner.GetOrIntern(relType)
	b.relTypes = append(b.relTypes, RelType(typeID))
	b.knownNodes.Add(u)
	b.knownNodes.Add(v)
	// Keep the lazy lookup map (if built) consistent; first-match wins for
	// a duplicate (u, v, type).
	if b.relIndex != nil {
		key := [3]uint32{u, v, typeID}
		if _, ok := b.relIndex[key]; !ok {
			b.relIndex[key] = idx
		}
	}
	return idx, nil
}

// stageValue resolves a convenience value, interning strings.
func (b *Builder) stageValue(value any) (Value, error) {
	switch v := value.(type) {
	case Value:
		return v, nil
	case string:
		return StrValue(b.interner.GetOrIntern(v)), nil
	case int64:
		return I64Value(v), nil
	case int:
		return I64Value(int64(v)), nil
	case int32:
		return I64Value(int64(v)), nil
	case float64:
		return F64Value(v), nil
	case bool:
		return BoolValue(v), nil
	}
	return Value{}, fmt.Errorf("%w: %T", ErrBadValue, value)
}

// InternPropertyKey interns a property-key name (or any string) and returns
// its atom, for reuse with SetPropByKey across many rows.
func (b *Builder) InternPropertyKey(name string) PropertyKey {
	return b.interner.GetOrIntern(name)
}

// SetProp stages a node property of any supported type (string, int,
// int32, int64, float64, bool, or Value), auto-typed into the matching
// column. The key interns before the value, matching the Rust typed
// setters' atom order.
func (b *Builder) SetProp(node NodeID, key string, value any) error {
	k := b.interner.GetOrIntern(key)
	return b.SetPropByKey(node, k, value)
}

// SetPropByKey is SetProp with a pre-interned key (see InternPropertyKey).
func (b *Builder) SetPropByKey(node NodeID, key PropertyKey, value any) error {
	v, err := b.stageValue(value)
	if err != nil {
		return err
	}
	if err := b.ensureCapacity(node); err != nil {
		return err
	}
	switch v.Kind() {
	case KindStr:
		atom, _ := v.StrID()
		b.nodeColStr[key] = append(b.nodeColStr[key], strPair{id: node, val: atom})
	case KindI64:
		x, _ := v.I64()
		b.nodeColI64[key] = append(b.nodeColI64[key], i64Pair{id: node, val: x})
	case KindF64:
		x, _ := v.F64()
		b.nodeColF64[key] = append(b.nodeColF64[key], f64Pair{id: node, val: x})
	case KindBool:
		x, _ := v.Bool()
		b.nodeColBool[key] = append(b.nodeColBool[key], boolPair{id: node, val: x})
	}
	b.knownNodes.Add(node)
	return nil
}

// UpdateProp replaces node's staged value for key (in the column matching
// the new value's type) rather than staging a duplicate write.
func (b *Builder) UpdateProp(node NodeID, key string, value any) error {
	k := b.interner.GetOrIntern(key)
	v, err := b.stageValue(value)
	if err != nil {
		return err
	}
	switch v.Kind() {
	case KindStr:
		b.nodeColStr[k] = removePair(b.nodeColStr[k], node)
	case KindI64:
		b.nodeColI64[k] = removePair(b.nodeColI64[k], node)
	case KindF64:
		b.nodeColF64[k] = removePair(b.nodeColF64[k], node)
	case KindBool:
		b.nodeColBool[k] = removePair(b.nodeColBool[k], node)
	}
	return b.SetPropByKey(node, k, v)
}

// removePair swap-removes the first pair staged for id.
func removePair[P interface{ pairID() uint32 }](pairs []P, id uint32) []P {
	for i, p := range pairs {
		if p.pairID() == id {
			pairs[i] = pairs[len(pairs)-1]
			return pairs[:len(pairs)-1]
		}
	}
	return pairs
}

func (p i64Pair) pairID() uint32  { return p.id }
func (p f64Pair) pairID() uint32  { return p.id }
func (p boolPair) pairID() uint32 { return p.id }
func (p strPair) pairID() uint32  { return p.id }

// findRelIndex resolves (u, v, relType) to the FIRST matching rel index,
// building the lazy lookup map on first use.
func (b *Builder) findRelIndex(u, v NodeID, relType string) (int, bool) {
	typeID, ok := b.interner.Get(relType)
	if !ok {
		return 0, false
	}
	if b.relIndex == nil {
		b.relIndex = make(map[[3]uint32]int, len(b.rels))
		for idx, r := range b.rels {
			key := [3]uint32{r[0], r[1], b.relTypes[idx].ID()}
			if _, seen := b.relIndex[key]; !seen {
				b.relIndex[key] = idx
			}
		}
	}
	idx, ok := b.relIndex[[3]uint32{u, v, typeID}]
	return idx, ok
}

// SetRelProp stages a property on the first rel matching (u, v, relType).
// For parallel rels (same endpoints and type), address the specific rel by
// the index AddRel returned via SetRelPropAt.
func (b *Builder) SetRelProp(u, v NodeID, relType, key string, value any) error {
	idx, ok := b.findRelIndex(u, v, relType)
	if !ok {
		return fmt.Errorf("%w: (%d)-[:%s]->(%d)", ErrRelNotFound, u, relType, v)
	}
	return b.SetRelPropAt(idx, key, value)
}

// SetRelPropAt stages a property on the rel at the given index (as returned
// by AddRel).
func (b *Builder) SetRelPropAt(relIdx int, key string, value any) error {
	if relIdx < 0 || relIdx >= len(b.rels) {
		return fmt.Errorf("%w: rel index %d out of range (%d rels)", ErrRelNotFound, relIdx, len(b.rels))
	}
	k := b.interner.GetOrIntern(key)
	v, err := b.stageValue(value)
	if err != nil {
		return err
	}
	idx := uint32(relIdx)
	switch v.Kind() {
	case KindStr:
		atom, _ := v.StrID()
		b.relColStr[k] = append(b.relColStr[k], strPair{id: idx, val: atom})
	case KindI64:
		x, _ := v.I64()
		b.relColI64[k] = append(b.relColI64[k], i64Pair{id: idx, val: x})
	case KindF64:
		x, _ := v.F64()
		b.relColF64[k] = append(b.relColF64[k], f64Pair{id: idx, val: x})
	case KindBool:
		x, _ := v.Bool()
		b.relColBool[k] = append(b.relColBool[k], boolPair{id: idx, val: x})
	}
	return nil
}

// NodeCount is the number of distinct nodes staged so far.
func (b *Builder) NodeCount() int {
	return int(b.knownNodes.GetCardinality())
}

// RelCount is the number of relationships staged so far.
func (b *Builder) RelCount() int {
	return len(b.rels)
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
// incoming for Both) -- an O(rels) pre-finalization scan.
func (b *Builder) NeighborIDs(node NodeID, dir Direction) []NodeID {
	var out []NodeID
	if dir == Outgoing || dir == Both {
		for _, r := range b.rels {
			if r[0] == node {
				out = append(out, r[1])
			}
		}
	}
	if dir == Incoming || dir == Both {
		for _, r := range b.rels {
			if r[1] == node {
				out = append(out, r[0])
			}
		}
	}
	return out
}
