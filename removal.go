// Builder removal surface: property removal (node and rel), rel tombstones,
// and detach-delete of nodes. Removals stay cheap at call time -- rels are
// tombstoned rather than compacted so handed-out rel indexes stay stable,
// and detach-delete defers the incident-rel cascade to Finalize (no per-node
// rel index exists, so an eager cascade would be O(m) per call).
//
// Miss-reporting convention, uniform across the family: every method
// reports whether it changed staged state as a bool, and an error is
// reserved for a dangling rel handle -- a rel index (returned by AddRel)
// or (u, v, type) address that is out of range, tombstoned, or dead via a
// detach-deleted endpoint is ErrRelNotFound. Node ids are open-world
// (AddNodeWithID stages arbitrary ids), so an unknown node is a plain
// miss, never an error.

package chickpeas

import (
	"fmt"

	"github.com/RoaringBitmap/roaring/v2"
)

// removeAllPairs filters every pair staged for id, preserving staging order,
// reporting whether any was removed.
func removeAllPairs[P interface{ pairID() uint32 }](pairs []P, id uint32) ([]P, bool) {
	out := pairs[:0]
	for _, p := range pairs {
		if p.pairID() != id {
			out = append(out, p)
		}
	}
	return out, len(out) != len(pairs)
}

// sweepPairs applies removeAllPairs to one key of a typed column map,
// dropping the key entirely when its last pair goes (an empty column must
// not finalize).
func sweepPairs[P interface{ pairID() uint32 }](cols map[PropertyKey][]P, key PropertyKey, id uint32) bool {
	pairs, ok := cols[key]
	if !ok {
		return false
	}
	out, removed := removeAllPairs(pairs, id)
	if len(out) == 0 {
		delete(cols, key)
	} else {
		cols[key] = out
	}
	return removed
}

// removeNodePropPairsByKey sweeps node's staged pairs for key across all
// four typed columns -- a key staged under multiple value types loses every
// staged occurrence, enforcing last-write-wins across types (Finalize's four
// column loops would otherwise resolve a cross-type duplicate by loop order,
// not write order).
func (b *Builder) removeNodePropPairsByKey(node NodeID, key PropertyKey) bool {
	removed := sweepPairs(b.nodeColI64, key, node)
	removed = sweepPairs(b.nodeColF64, key, node) || removed
	removed = sweepPairs(b.nodeColBool, key, node) || removed
	removed = sweepPairs(b.nodeColStr, key, node) || removed
	if removed {
		b.markNodeColDirty(key)
	}
	return removed
}

// RemoveProp deletes node's staged property key, sweeping every staged
// occurrence across all four typed columns (duplicate staged writes and
// cross-type stagings all go -- a partial sweep would resurrect a stale
// value at Finalize). Reports whether anything was removed; false covers
// every kind of miss uniformly (a key never staged anywhere, or staged but
// not on this node -- either way no staged pair existed for (node, key)).
func (b *Builder) RemoveProp(node NodeID, key string) bool {
	k, ok := b.interner.Get(key)
	if !ok {
		return false
	}
	return b.removeNodePropPairsByKey(node, k)
}

// removeNodePropPairs purges every staged property pair of node across all
// keys (the detach-delete property sweep). Only the keys that actually held a
// pair for node are marked dirty; the rest still alias at Finalize, since a
// node the column never covered leaves the rebuilt column unchanged.
func (b *Builder) removeNodePropPairs(node NodeID) {
	sweep := func(removed bool, key PropertyKey) {
		if removed {
			b.markNodeColDirty(key)
		}
	}
	for key := range b.nodeColI64 {
		sweep(sweepPairs(b.nodeColI64, key, node), key)
	}
	for key := range b.nodeColF64 {
		sweep(sweepPairs(b.nodeColF64, key, node), key)
	}
	for key := range b.nodeColBool {
		sweep(sweepPairs(b.nodeColBool, key, node), key)
	}
	for key := range b.nodeColStr {
		sweep(sweepPairs(b.nodeColStr, key, node), key)
	}
}

// RemoveRelProp deletes the staged property key on the first rel matching
// (u, v, relType) -- the addressing dual of SetRelProp. For parallel rels,
// address the specific rel via RemoveRelPropAt. Reports whether anything
// was removed; an unmatched (u, v, relType) address is ErrRelNotFound.
func (b *Builder) RemoveRelProp(u, v NodeID, relType, key string) (bool, error) {
	idx, ok := b.findRelIndex(u, v, relType)
	if !ok {
		return false, fmt.Errorf("%w: (%d)-[:%s]->(%d)", ErrRelNotFound, u, relType, v)
	}
	return b.RemoveRelPropAt(idx, key)
}

// RemoveRelPropAt deletes the staged property key on the rel at relIdx (as
// returned by AddRel), sweeping every staged occurrence across all four
// typed columns. Reports whether anything was removed -- a key with no
// staged pair on this rel is (false, nil), distinguishable from a real
// removal; a removed or out-of-range rel is ErrRelNotFound.
func (b *Builder) RemoveRelPropAt(relIdx int, key string) (bool, error) {
	if relIdx < 0 || relIdx >= len(b.rels) || b.relRemoved(relIdx) {
		return false, fmt.Errorf("%w: rel index %d", ErrRelNotFound, relIdx)
	}
	k, ok := b.interner.Get(key)
	if !ok {
		return false, nil
	}
	id := uint32(relIdx)
	removed := sweepPairs(b.relColI64, k, id)
	removed = sweepPairs(b.relColF64, k, id) || removed
	removed = sweepPairs(b.relColBool, k, id) || removed
	removed = sweepPairs(b.relColStr, k, id) || removed
	if removed {
		b.markRelColDirty(k)
	}
	return removed, nil
}

// RemoveRel tombstones the rel at relIdx (as returned by AddRel). The
// staging array is never compacted -- swap-removal would invalidate every
// handed-out rel index and the staged rel-prop ids -- so the rel is marked
// removed, degrees adjust immediately, and Finalize compacts in one pass.
// Removing a rel that is out of range, already removed, or dead via a
// detach-deleted endpoint is ErrRelNotFound (no removed bool: a live
// handle always removes, so removal happened exactly when err is nil).
func (b *Builder) RemoveRel(relIdx int) error {
	if relIdx < 0 || relIdx >= len(b.rels) || b.relRemoved(relIdx) {
		return fmt.Errorf("%w: rel index %d", ErrRelNotFound, relIdx)
	}
	if b.removedRels == nil {
		b.removedRels = roaring.New()
	}
	b.markRelsDirty()
	b.removedRels.Add(uint32(relIdx))
	r := b.rels[relIdx]
	b.degOut[r[0]]--
	b.degIn[r[1]]--
	b.relIndex = nil // lazily rebuilds skipping tombstones
	return nil
}

// RemoveNode detach-deletes node: its labels and staged properties go
// immediately, and every currently staged incident rel (with its rel
// properties) dies at Finalize. Reports whether the node was known.
//
// Ids retire rather than reuse -- nextNodeID never rewinds -- but removal is
// not a permanent tombstone: any later staging touch (AddNodeWithID,
// SetProp, or being an AddRel endpoint) resurrects the id as a fresh
// unlabeled, propertyless node. Rels staged before the removal stay dead
// either way; only rels added after the resurrection survive.
func (b *Builder) RemoveNode(id NodeID) bool {
	if !b.knownNodes.Contains(id) {
		return false
	}
	b.knownNodes.Remove(id)
	if int(id) < len(b.nodeLabels) {
		for _, l := range b.nodeLabels[id] {
			b.markLabelDirty(l)
		}
		b.nodeLabels[id] = nil
	}
	// The Finalize cascade rescans every staged rel for incidence, so the rel
	// set is treated as dirty even when the node turns out to be isolated.
	b.markRelsDirty()
	b.removeNodePropPairs(id)
	if b.removedNodes == nil {
		b.removedNodes = map[NodeID]int{}
	}
	// The watermark (staged rel count at removal time) scopes the Finalize
	// cascade: rels below it die, rels added afterwards -- which resurrect
	// the node -- survive.
	b.removedNodes[id] = len(b.rels)
	b.relIndex = nil // may reference rels the cascade will kill
	return true
}

// relRemoved reports whether the staged rel at idx is dead -- explicitly
// tombstoned, or staged before a detach-delete of either endpoint.
func (b *Builder) relRemoved(idx int) bool {
	if b.removedRels != nil && b.removedRels.Contains(uint32(idx)) {
		return true
	}
	if len(b.removedNodes) > 0 {
		r := b.rels[idx]
		if wm, ok := b.removedNodes[r[0]]; ok && idx < wm {
			return true
		}
		if wm, ok := b.removedNodes[r[1]]; ok && idx < wm {
			return true
		}
	}
	return false
}

// hasRemovals reports whether any removal state is pending compaction.
func (b *Builder) hasRemovals() bool {
	return (b.removedRels != nil && !b.removedRels.IsEmpty()) || len(b.removedNodes) > 0
}

// compactRemovals folds pending removals into the staging state in one
// O(m + pairs) pass: dead rels (explicit tombstones plus the detach-delete
// cascade) drop out in staging order, degrees recompute, and staged rel-prop
// pairs remap to the compacted indexes or drop with their rel. Called by
// Finalize before any build phase; a no-op without removals.
func (b *Builder) compactRemovals() {
	if !b.hasRemovals() {
		return
	}
	m := len(b.rels)
	oldToNew := make([]int32, m)
	kept := 0
	for idx := range m {
		if b.relRemoved(idx) {
			oldToNew[idx] = -1
			continue
		}
		oldToNew[idx] = int32(kept)
		b.rels[kept] = b.rels[idx]
		b.relTypes[kept] = b.relTypes[idx]
		kept++
	}
	b.rels = b.rels[:kept]
	b.relTypes = b.relTypes[:kept]
	clear(b.degOut)
	clear(b.degIn)
	for _, r := range b.rels {
		b.degOut[r[0]]++
		b.degIn[r[1]]++
	}
	remapRelPairs(b.relColI64, oldToNew)
	remapRelPairs(b.relColF64, oldToNew)
	remapRelPairs(b.relColBool, oldToNew)
	remapRelPairs(b.relColStr, oldToNew)
	b.removedRels = nil
	b.removedNodes = nil
	b.relIndex = nil
}

// relPair constrains the four staged pair types for compaction remapping.
type relPair[P any] interface {
	pairID() uint32
	withPairID(id uint32) P
}

// remapRelPairs rewrites staged rel-prop ids through the compaction map,
// dropping pairs on dead rels and keys left empty.
func remapRelPairs[P relPair[P]](cols map[PropertyKey][]P, oldToNew []int32) {
	for key, pairs := range cols {
		out := pairs[:0]
		for _, p := range pairs {
			ni := oldToNew[p.pairID()]
			if ni < 0 {
				continue
			}
			out = append(out, p.withPairID(uint32(ni)))
		}
		if len(out) == 0 {
			delete(cols, key)
		} else {
			cols[key] = out
		}
	}
}
