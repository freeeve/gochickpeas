// Property, column, rel-property, and atom accessors over a Snapshot.

package chickpeas

import (
	"sort"
	"sync"
)

// Prop reads node's property key, chaining into typed reads:
// g.Prop(n, "age").I64Or(0). The zero Prop means absent.
func (g *Snapshot) Prop(node NodeID, key string) Prop {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Prop{}
	}
	column, ok := g.columns[keyID]
	if !ok {
		return Prop{}
	}
	v, ok := column.Get(node)
	if !ok {
		return Prop{}
	}
	return Prop{g: g, v: v, ok: true}
}

// RelProp reads a relationship property by outgoing-CSR position (as
// carried by RelRef.Pos) -- the rel analogue of Prop.
func (g *Snapshot) RelProp(pos uint32, key string) Prop {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Prop{}
	}
	column, ok := g.relColumns[keyID]
	if !ok {
		return Prop{}
	}
	v, ok := column.Get(pos)
	if !ok {
		return Prop{}
	}
	return Prop{g: g, v: v, ok: true}
}

// RelEndpoints returns the (source, target) node ids of the rel at
// outgoing-CSR position pos -- the tail and head as stored, independent of
// the traversal direction that produced the position (backs startNode/
// endNode). O(log n): the target indexes outNbrs directly; the source is
// the node whose offset range contains pos.
func (g *Snapshot) RelEndpoints(pos uint32) (source, target NodeID, ok bool) {
	p := int(pos)
	if p >= len(g.outNbrs) {
		return 0, 0, false
	}
	target = g.outNbrs[p]
	// The source is the last node whose CSR range starts at or before pos;
	// empty ranges share an offset, so a <= partition picks the owner.
	i := sort.Search(len(g.outOffsets), func(i int) bool { return int(g.outOffsets[i]) > p })
	if i == 0 {
		return 0, 0, false
	}
	return NodeID(i - 1), target, true
}

// RelTypeAt returns the type name of the relationship at outgoing-CSR
// position pos (backs the query engine's type(r)). Every relationship has
// exactly one type, so ok is a BOUNDS guard only -- false means "pos does
// not index a relationship", never "this relationship has no type" -- which
// is why it mirrors RelEndpoints rather than the semantically-fallible
// RelProp. O(1): a direct index into the outgoing-CSR type array, then the
// atom table resolves the id to its interned name.
func (g *Snapshot) RelTypeAt(pos uint32) (string, bool) {
	if int(pos) >= len(g.outTypes) {
		return "", false
	}
	return g.atoms.Resolve(uint32(g.outTypes[pos]))
}

// Col resolves a reader for the node property key; ok is false when no such
// column exists. Narrow with I64/F64/Bool/Str and hoist out of a hot loop
// instead of calling Prop per node.
func (g *Snapshot) Col(key string) (Col, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Col{}, false
	}
	column, ok := g.columns[keyID]
	if !ok {
		return Col{}, false
	}
	return Col{col: column}, true
}

// ColIndexed is Col with O(1) reads at scattered node ids even when the
// column is stored sparse: a position -> slot index is built once (lazily,
// cached on the snapshot) and shared by every reader. Use in hot kernels
// reading a sparse column at many positions; dense columns are already O(1)
// and ignore the index.
func (g *Snapshot) ColIndexed(key string) (Col, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Col{}, false
	}
	column, ok := g.columns[keyID]
	if !ok {
		return Col{}, false
	}
	return Col{col: column, idx: g.posIndexFor(keyID, column, &g.colPosMu, g.colPosIndex)}, true
}

// RelCol resolves a reader for the relationship property key, indexed by
// outgoing-CSR position (see RelRef.Pos); ok is false when absent.
func (g *Snapshot) RelCol(key string) (Col, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Col{}, false
	}
	column, ok := g.relColumns[keyID]
	if !ok {
		return Col{}, false
	}
	return Col{col: column}, true
}

// RelColIndexed is RelCol with O(1) sparse reads -- ideal for per-rel
// weight reads inside a search.
func (g *Snapshot) RelColIndexed(key string) (Col, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return Col{}, false
	}
	column, ok := g.relColumns[keyID]
	if !ok {
		return Col{}, false
	}
	return Col{col: column, idx: g.posIndexFor(keyID, column, &g.relColPosMu, g.relColPosIndex)}, true
}

// posIndexFor lazily builds (once) and returns the position -> slot index
// for a sparse column; dense columns are already O(1) and get nil. The
// table is insert-only and an entry, once built, is never replaced, so the
// returned map reads safely off-lock.
func (g *Snapshot) posIndexFor(key PropertyKey, column Column, mu *sync.Mutex, table map[PropertyKey]posIndex) posIndex {
	if !isSparse(column) {
		return nil
	}
	mu.Lock()
	idx, ok := table[key]
	mu.Unlock()
	if ok {
		return idx
	}
	built := buildPosIndex(column)
	mu.Lock()
	defer mu.Unlock()
	if existing, ok := table[key]; ok {
		return existing
	}
	table[key] = built
	return built
}

// NodePropertyKeys lists the property keys node carries a value for, in
// ascending key order -- the reverse of Prop (backs keys(n)/properties(n)).
// Sorted so the enumeration is deterministic.
func (g *Snapshot) NodePropertyKeys(node NodeID) []string {
	var keys []string
	for key, column := range g.columns {
		if _, ok := column.Get(node); ok {
			if name, found := g.atoms.Resolve(key); found {
				keys = append(keys, name)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// Version is the snapshot's version string; ok is false when none was set.
func (g *Snapshot) Version() (string, bool) {
	if g.version == nil {
		return "", false
	}
	return *g.version, true
}

// Atoms is the snapshot's interned-string table (atom <-> string in both
// directions).
func (g *Snapshot) Atoms() *Atoms {
	return g.atoms
}

// ResolveString resolves an atom id to its string; ok is false when out of
// range.
func (g *Snapshot) ResolveString(id uint32) (string, bool) {
	return g.atoms.Resolve(id)
}

// PropertyKey resolves a property-key name to its atom; ok is false when
// the key was never interned.
func (g *Snapshot) PropertyKey(key string) (PropertyKey, bool) {
	return g.atoms.ID(key)
}

// Label resolves a label name to its atom; ok is false when unknown.
func (g *Snapshot) Label(name string) (Label, bool) {
	id, ok := g.atoms.ID(name)
	return Label(id), ok
}

// RelType resolves a relationship-type name to its atom; ok is false when
// unknown. Resolve once and pass to MatchType in a hot loop to skip the
// per-call string lookup.
func (g *Snapshot) RelType(name string) (RelType, bool) {
	id, ok := g.atoms.ID(name)
	return RelType(id), ok
}

// ValueFromString resolves a string property value to its interned Value;
// ok is false when the string was never interned (so no property equals
// it).
func (g *Snapshot) ValueFromString(s string) (Value, bool) {
	id, ok := g.atoms.ID(s)
	return StrValue(id), ok
}
