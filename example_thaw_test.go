package chickpeas_test

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
)

// Example_managerWriteLoop demonstrates the read-modify-refinalize-swap
// write pattern: thaw the current snapshot into a builder, edit it
// (additions and removals alike), Finalize a new immutable snapshot, and
// register it in the Manager. Readers keep whatever snapshot they already
// hold -- the registry swap is the commit point, and no snapshot is ever
// mutated.
func Example_managerWriteLoop() {
	// Boot: build the initial snapshot and register it.
	b := chickpeas.NewBuilder(0, 0)
	alice, _ := b.AddNode("Person")
	bob, _ := b.AddNode("Person")
	b.SetProp(alice, "name", "alice")
	b.SetProp(bob, "name", "bob")
	b.AddRel(alice, bob, "KNOWS")

	m := chickpeas.NewManager()
	m.AddSnapshotWithVersion("v1", b.Finalize())

	// One write cycle: read -> thaw -> edit -> Finalize -> swap.
	cur, _ := m.Snapshot("v1")
	w := chickpeas.NewBuilderFromSnapshot(cur)
	carol, _ := w.AddNode("Person")
	w.SetProp(carol, "name", "carol")
	w.AddRel(alice, carol, "KNOWS")
	w.RemoveNode(bob) // detach-delete: bob's rels die with him
	m.AddSnapshotWithVersion("v2", w.Finalize())

	v1, _ := m.Snapshot("v1")
	v2, _ := m.Snapshot("v2")
	fmt.Printf("v1: %d nodes, %d rels\n", v1.NodeCount(), v1.RelCount())
	fmt.Printf("v2: %d nodes, %d rels\n", v2.NodeCount(), v2.RelCount())
	for nbr := range v2.Neighbors(alice, chickpeas.Outgoing) {
		fmt.Printf("alice now knows %s\n", v2.Prop(nbr, "name").StrOr("?"))
	}
	// Output:
	// v1: 2 nodes, 1 rels
	// v2: 2 nodes, 1 rels
	// alice now knows carol
}
