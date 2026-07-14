// type(r): the relationship-type accessor, resolving a bound relationship's
// CSR position to its type name via the graph's RelTypeAt. strCol runs both
// the compiled and interpreted eval paths and asserts they agree, so this
// pins type(r) on both.
package gql

import "testing"

func TestTypeOfRelationship(t *testing.T) {
	g := socialGraph(t)
	// Alice's outgoing relationships span two types; type(r) resolves each.
	wantStrs(t, strCol(t, g,
		"MATCH (:Person {name: 'Alice'})-[r]->(x) RETURN type(r) AS t", "t"),
		"KNOWS", "KNOWS", "WORKS_AT")
	// A type-filtered rel resolves to that type.
	wantStrs(t, strCol(t, g,
		"MATCH (:Person {name: 'Alice'})-[r:WORKS_AT]->(c:Company) RETURN type(r) AS t", "t"),
		"WORKS_AT")
}
