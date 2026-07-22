package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// TestContainsNodeAndEdge covers the acyclic node-stack and trail edge-stack
// membership checks that reject a repeated node (ACYCLIC) or a repeated
// directed edge (TRAIL) during a variable-length walk.
func TestContainsNodeAndEdge(t *testing.T) {
	nodes := []graph.NodeID{1, 4, 9}
	if !containsNode(nodes, 4) {
		t.Fatal("4 is on the stack")
	}
	if containsNode(nodes, 5) {
		t.Fatal("5 is not on the stack")
	}
	if containsNode(nil, 1) {
		t.Fatal("an empty stack contains nothing")
	}

	edges := [][2]graph.NodeID{{1, 2}, {3, 4}}
	if !containsEdge(edges, [2]graph.NodeID{3, 4}) {
		t.Fatal("(3,4) is used")
	}
	// The ordered pair matters: (4,3) is a distinct directed edge.
	if containsEdge(edges, [2]graph.NodeID{4, 3}) {
		t.Fatal("(4,3) is a different ordered edge")
	}
	if containsEdge(edges, [2]graph.NodeID{1, 3}) {
		t.Fatal("(1,3) is not used")
	}
}
