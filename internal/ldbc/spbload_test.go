// Ports of the rustychickpeas-ldbc src/spb/loader.rs unit tests, plus
// the IRI canonicalization cases (UCHAR unescape + percent-decode) the
// SPB extract exercises.

package ldbc

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

const spbDoc = `
<http://ex/cw1> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://bbc/CreativeWork> .
<http://ex/cw1> <http://bbc/title> "Hello" .
<http://ex/cw1> <http://bbc/about> <http://ex/London> .
<http://ex/London> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://dbo/Place> .
<http://ex/London> <http://geo#lat> "51.5"^^<http://www.w3.org/2001/XMLSchema#double> .
<http://ex/London> <http://geo#views> "42"^^<http://www.w3.org/2001/XMLSchema#integer> .
`

// soleLabeled returns the single node with a label, failing otherwise.
func soleLabeled(t *testing.T, g *chickpeas.Snapshot, label string) chickpeas.NodeID {
	t.Helper()
	set, ok := g.NodesWithLabel(label)
	if !ok || set.Len() != 1 {
		t.Fatalf("label %s: want exactly 1 node, have %v", label, set)
	}
	for n := range set.Iter() {
		return n
	}
	return 0
}

func TestSPBLoadMapsTypesToLabelsRelsAndProps(t *testing.T) {
	g, stats := LoadSPBString(spbDoc)
	if stats.Resources != 2 {
		t.Fatalf("resources = %d, want 2", stats.Resources)
	}
	if stats.Rels != 1 {
		t.Fatalf("rels = %d, want 1", stats.Rels)
	}
	if stats.Literals < 3 {
		t.Fatalf("literals = %d, want >= 3", stats.Literals)
	}

	soleLabeled(t, g, "CreativeWork")
	london := soleLabeled(t, g, "Place")

	if v, ok := g.Prop(london, "lat").F64(); !ok || v != 51.5 {
		t.Fatalf("lat = %v %v, want 51.5", v, ok)
	}
	if v, ok := g.Prop(london, "views").I64(); !ok || v != 42 {
		t.Fatalf("views = %v %v, want 42", v, ok)
	}
	if u, ok := g.Prop(london, "uri").Str(); !ok || u != "http://ex/London" {
		t.Fatalf("uri = %q %v", u, ok)
	}
}

func TestSPBLoadRelUsesPredicateLocalName(t *testing.T) {
	g, _ := LoadSPBString(spbDoc)
	cw := soleLabeled(t, g, "CreativeWork")
	var about []chickpeas.NodeID
	for n := range g.Neighbors(cw, chickpeas.Outgoing, "about") {
		about = append(about, n)
	}
	if len(about) != 1 {
		t.Fatalf("about neighbors = %d, want 1", len(about))
	}
}

func TestSPBLoadRDFSForwardChaining(t *testing.T) {
	const doc = `
<http://bbc/BlogPost> <http://www.w3.org/2000/01/rdf-schema#subClassOf> <http://bbc/CreativeWork> .
<http://bbc/CreativeWork> <http://www.w3.org/2000/01/rdf-schema#subClassOf> <http://www.w3.org/2002/07/owl#Thing> .
<http://dbo/Company> <http://www.w3.org/2000/01/rdf-schema#subClassOf> <http://cc/Thing> .
<http://bbc/about> <http://www.w3.org/2000/01/rdf-schema#subPropertyOf> <http://bbc/tag> .
<http://ex/cw1> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://bbc/BlogPost> .
<http://ex/cw1> <http://bbc/about> <http://ex/Acme> .
<http://ex/Acme> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://dbo/Company> .
`
	g, stats := LoadSPBString(doc)
	if stats.Resources != 2 {
		t.Fatalf("resources = %d, want 2 (TBox is not instance data)", stats.Resources)
	}

	cw1 := soleLabeled(t, g, "BlogPost")
	if !g.HasLabel(cw1, "CreativeWork") {
		t.Fatal("subClassOf: cw1 should also be a CreativeWork")
	}
	acme := soleLabeled(t, g, "Company")
	if !g.HasLabel(acme, "Thing") {
		t.Fatal("subClassOf: Acme should also be a Thing (coreconcepts)")
	}
	if g.HasLabel(cw1, "Thing") {
		t.Fatal("owl:Thing must not materialize as a label")
	}

	var tag, about []chickpeas.NodeID
	for n := range g.Neighbors(cw1, chickpeas.Outgoing, "tag") {
		tag = append(tag, n)
	}
	for n := range g.Neighbors(cw1, chickpeas.Outgoing, "about") {
		about = append(about, n)
	}
	if len(tag) != 1 || len(about) != 1 || tag[0] != about[0] {
		t.Fatalf("subPropertyOf: tag=%v about=%v, want the same single rel", tag, about)
	}
}

func TestSPBLoadCanonicalizesIRISpellings(t *testing.T) {
	// The same entity written percent-encoded, UCHAR-escaped, and raw
	// must intern to one node; the uri property carries the decoded form.
	const doc = `
<http://ex/w1> <http://bbc/about> <http://db/Ottoman%E2%80%93Portuguese> .
<http://ex/w2> <http://bbc/about> <http://db/Ottoman–Portuguese> .
<http://db/Ottoman–Portuguese> <http://bbc/label> "x" .
`
	g, stats := LoadSPBString(doc)
	if stats.Resources != 3 {
		t.Fatalf("resources = %d, want 3 (w1, w2, one Ottoman node)", stats.Resources)
	}
	found := false
	for id := range chickpeas.NodeID(3) {
		if u, _ := g.Prop(id, "uri").Str(); u == "http://db/Ottoman–Portuguese" {
			found = true
		}
	}
	if !found {
		t.Fatal("decoded uri property missing")
	}
}

func TestSPBPercentDecodeEdgeCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://ex/plain", "http://ex/plain"},
		{"http://ex/a%20b", "http://ex/a b"},
		{"http://ex/bad%zz", "http://ex/bad%zz"},   // invalid hex: verbatim
		{"http://ex/trunc%2", "http://ex/trunc%2"}, // truncated: verbatim
		{"http://ex/%E2%80%93", "http://ex/–"},     // multi-byte UTF-8
		{"http://ex/lone%ff", "http://ex/lone%ff"}, // invalid UTF-8: whole input verbatim
	}
	for _, c := range cases {
		if got := spbPercentDecode(c.in); got != c.want {
			t.Errorf("percentDecode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSPBLocalNameMatchesRust(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://www.w3.org/1999/02/22-rdf-syntax-ns#type", "type"},
		{"http://dbpedia.org/ontology/Place", "Place"},
		{"http://ex/geo#long", "long"},
		{"http://ex#a/b", "b"}, // hash first, then slash -- the Rust order
	}
	for _, c := range cases {
		if got := spbLocalName(c.in); got != c.want {
			t.Errorf("localName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSPBLiteralFirstWins(t *testing.T) {
	const doc = `
<http://ex/n> <http://bbc/title> "first" .
<http://ex/n> <http://bbc/title> "second" .
`
	g, stats := LoadSPBString(doc)
	if stats.Literals != 1 {
		t.Fatalf("literals = %d, want 1 (first wins)", stats.Literals)
	}
	if v, _ := g.Prop(0, "title").Str(); v != "first" {
		t.Fatalf("title = %q, want first", v)
	}
}
