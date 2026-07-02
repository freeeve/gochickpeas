// The lazy per-value equality index behind NodesWithProperty and friends:
// (label, key) -> value -> node set, built on first access by scanning the
// column, with builds running outside the lock (a racing duplicate is
// discarded -- both are identical).

package chickpeas

import (
	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/nodeset"
)

type propIndexKey struct {
	label Label
	key   PropertyKey
}

// anyLabel is the reserved sentinel scoping the label-free index built by
// NodeWithProperty.
const anyLabel = Label(^uint32(0))

// valueFromAny resolves a convenience value (string, int, int32, int64,
// float64, bool, or Value) to a Value. A string that was never interned
// resolves to nothing -- no property can equal it.
func (g *Snapshot) valueFromAny(value any) (Value, bool) {
	switch v := value.(type) {
	case Value:
		return v, true
	case string:
		return g.ValueFromString(v)
	case int64:
		return I64Value(v), true
	case int:
		return I64Value(int64(v)), true
	case int32:
		return I64Value(int64(v)), true
	case float64:
		return F64Value(v), true
	case bool:
		return BoolValue(v), true
	}
	return Value{}, false
}

// buildPropValueIndex groups a column's entries by value, restricted to
// labelNodes when non-nil.
func buildPropValueIndex(column Column, labelNodes *nodeset.Set) map[Value]*nodeset.Set {
	grouped := map[Value][]uint32{}
	for pos, v := range column.Entries() {
		if labelNodes == nil || labelNodes.Contains(pos) {
			grouped[v] = append(grouped[v], pos)
		}
	}
	out := make(map[Value]*nodeset.Set, len(grouped))
	for v, ids := range grouped {
		bm := roaring.New()
		bm.AddMany(ids)
		out[v] = nodeset.FromBitmap(bm)
	}
	return out
}

// withPropValueIndex runs query against the cached per-value index for
// indexKey, building it once via build on a miss. The build runs OUTSIDE
// the lock; build returning nil (unknown column/label) short-circuits.
func (g *Snapshot) withPropValueIndex(
	indexKey propIndexKey,
	build func() map[Value]*nodeset.Set,
	query func(map[Value]*nodeset.Set) (*nodeset.Set, bool),
) (*nodeset.Set, bool) {
	g.propIndexMu.Lock()
	cached, ok := g.propIndex[indexKey]
	g.propIndexMu.Unlock()
	if ok {
		return query(cached)
	}
	built := build()
	if built == nil {
		return nil, false
	}
	g.propIndexMu.Lock()
	if existing, ok := g.propIndex[indexKey]; ok {
		built = existing
	} else {
		g.propIndex[indexKey] = built
	}
	g.propIndexMu.Unlock()
	return query(built)
}

// NodesWithValue is the set of label-carrying nodes whose property key
// equals v -- the typed core of NodesWithProperty. The index for (label,
// key) is built lazily on first access and cached. The returned set is
// shared -- callers must not mutate it (Clone first). ok is false when the
// label/key is unknown or no node carries that value.
func (g *Snapshot) NodesWithValue(label, key string, v Value) (*nodeset.Set, bool) {
	labelID, ok := g.Label(label)
	if !ok {
		return nil, false
	}
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return nil, false
	}
	return g.withPropValueIndex(
		propIndexKey{label: labelID, key: keyID},
		func() map[Value]*nodeset.Set {
			labelNodes, ok := g.labelIndex[labelID]
			if !ok {
				return nil
			}
			column, ok := g.columns[keyID]
			if !ok {
				return nil
			}
			return buildPropValueIndex(column, labelNodes)
		},
		func(index map[Value]*nodeset.Set) (*nodeset.Set, bool) {
			set, ok := index[v]
			return set, ok
		},
	)
}

// NodesWithProperty is NodesWithValue with a convenience value (string,
// int, int32, int64, float64, bool, or Value).
func (g *Snapshot) NodesWithProperty(label, key string, value any) (*nodeset.Set, bool) {
	v, ok := g.valueFromAny(value)
	if !ok {
		return nil, false
	}
	return g.NodesWithValue(label, key, v)
}

// NodeWithProperty finds a single node by a property value across ALL
// labels (unlike the label-scoped NodesWithProperty) -- for unique keys
// such as a uri. Returns the smallest matching node id. The label-free
// (key, value) index is built lazily and cached under a reserved sentinel.
func (g *Snapshot) NodeWithProperty(key string, value any) (NodeID, bool) {
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return 0, false
	}
	v, ok := g.valueFromAny(value)
	if !ok {
		return 0, false
	}
	set, ok := g.withPropValueIndex(
		propIndexKey{label: anyLabel, key: keyID},
		func() map[Value]*nodeset.Set {
			column, ok := g.columns[keyID]
			if !ok {
				return nil
			}
			return buildPropValueIndex(column, nil)
		},
		func(index map[Value]*nodeset.Set) (*nodeset.Set, bool) {
			set, ok := index[v]
			return set, ok
		},
	)
	if !ok {
		return 0, false
	}
	return set.Min()
}

// NodeWithLabelProperty finds a single label-carrying node whose property
// key equals value -- the label-scoped sibling of NodeWithProperty, for
// keys unique only within a label. Returns the smallest matching node id;
// reuses the cached (label, key) index NodesWithProperty builds.
func (g *Snapshot) NodeWithLabelProperty(label, key string, value any) (NodeID, bool) {
	set, ok := g.NodesWithProperty(label, key, value)
	if !ok {
		return 0, false
	}
	return set.Min()
}
