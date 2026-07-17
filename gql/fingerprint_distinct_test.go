// Template-fingerprint distinctness (task 200): a query pair differing
// only in a construct the fingerprint cannot see shares one cached
// template plan -- a CORRECTNESS bug, found live twice when Run went
// through the default cache (REPEATABLE ELEMENTS colliding with its
// TRAIL twin; 1 IS TYPED FLOAT answering with 1.5's baked result). Every
// pair here must fingerprint apart after desugar + auto-parameterization,
// exactly the cache's L2 keying.
package gql

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

func fpOf(t *testing.T, q string) string {
	t.Helper()
	qs, err := parseDesugar(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	semantics.AutoParameterize(qs)
	return ast.Fingerprint(qs)
}

func TestFingerprintDistinguishesSemantics(t *testing.T) {
	pairs := [][2]string{
		{"MATCH (x)-[:R]-(y)-[:R]-(z) RETURN count(*) AS n",
			"MATCH REPEATABLE ELEMENTS (x)-[:R]-(y)-[:R]-(z) RETURN count(*) AS n"},
		{"RETURN 1 IS TYPED FLOAT AS x", "RETURN 1 IS TYPED INTEGER AS x"},
		{"RETURN true IS TRUE AS x", "RETURN true IS FALSE AS x"},
		{"RETURN true IS TRUE AS x", "RETURN true IS NOT TRUE AS x"},
		{"RETURN 1 IS TYPED FLOAT AS x", "RETURN 1 IS NOT TYPED FLOAT AS x"},
		{"RETURN 'a' IS NORMALIZED AS x", "RETURN 'a' IS NFD NORMALIZED AS x"},
	}
	for _, p := range pairs {
		if fpOf(t, p[0]) == fpOf(t, p[1]) {
			t.Errorf("fingerprint collision:\n  %s\n  %s", p[0], p[1])
		}
	}
}
