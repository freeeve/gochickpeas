// Engine scalar-type re-exports, so seam consumers (plan/eval/exec) can
// name node ids and directions without importing the engine package
// directly -- the interface remains the only coupling point.
package graph

import chickpeas "github.com/freeeve/gochickpeas"

// NodeID is the engine's node identifier.
type NodeID = chickpeas.NodeID

// Direction is the engine's traversal direction.
type Direction = chickpeas.Direction

// Direction values.
const (
	Outgoing = chickpeas.Outgoing
	Incoming = chickpeas.Incoming
	Both     = chickpeas.Both
)
