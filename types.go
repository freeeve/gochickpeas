// Package chickpeas is a high-performance in-memory graph database: the Go
// implementation of RustyChickpeas. Graphs are built with a Builder,
// finalized into an immutable read-optimized Snapshot (CSR adjacency,
// columnar properties, lazy indexes), and exchanged as RCPG files (package
// rcpg) byte-compatibly with the Rust implementation.
package chickpeas

// NodeID identifies a node. u32 bounds the graph at ~4.3B nodes and keeps
// roaring bitmaps and CSR arrays compact.
type NodeID = uint32

// PropertyKey is an interned property-key atom.
type PropertyKey = uint32

// Direction selects which adjacency a traversal follows.
type Direction uint8

const (
	// Outgoing follows rels from source to destination.
	Outgoing Direction = iota
	// Incoming follows rels from destination to source.
	Incoming
	// Both follows rels in either direction.
	Both
)

// Reverse flips Outgoing and Incoming; Both is its own reverse.
func (d Direction) Reverse() Direction {
	switch d {
	case Outgoing:
		return Incoming
	case Incoming:
		return Outgoing
	}
	return Both
}

// String implements fmt.Stringer.
func (d Direction) String() string {
	switch d {
	case Outgoing:
		return "outgoing"
	case Incoming:
		return "incoming"
	case Both:
		return "both"
	}
	return "invalid"
}

// Label is an interned node-label atom. Resolve text via the snapshot.
type Label uint32

// ID is the raw atom id.
func (l Label) ID() uint32 { return uint32(l) }

// RelType is an interned relationship-type atom. Resolve text via the
// snapshot.
type RelType uint32

// ID is the raw atom id.
func (t RelType) ID() uint32 { return uint32(t) }
