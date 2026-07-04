package chickpeas_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// nqTestGraph builds a snapshot exercising every write-side feature:
// sparse ids, multiple labels, all four value kinds, float specials,
// names needing IRI escaping, parallel rels, rel props, and a version.
func nqTestGraph(t testing.TB) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(0, 0)
	b.SetVersion("nq-test-v1")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	addNode := func(id chickpeas.NodeID, labels ...string) {
		t.Helper()
		if _, err := b.AddNodeWithID(id, labels...); err != nil {
			t.Fatal(err)
		}
	}
	addNode(0, "Person", "Admin")
	addNode(1, "Person")
	addNode(5)
	addNode(100, "Röle with space")
	must(b.SetProp(0, "name", "Alice"))
	must(b.SetProp(0, "age", int64(42)))
	must(b.SetProp(0, "score", 3.14))
	must(b.SetProp(0, "active", true))
	must(b.SetProp(1, "name", "Bob \"quoted\"\nsecond line"))
	must(b.SetProp(5, "has space%25", "odd key"))
	must(b.SetProp(100, "neg", int64(-7)))
	must(b.SetProp(100, "infp", math.Inf(1)))
	must(b.SetProp(100, "infn", math.Inf(-1)))
	must(b.SetProp(100, "nan", math.NaN()))
	r0, err := b.AddRel(0, 1, "KNOWS")
	must(err)
	must(b.SetRelPropAt(r0, "weight", 0.5))
	must(b.SetRelPropAt(r0, "since", int64(2020)))
	r1, err := b.AddRel(0, 1, "KNOWS")
	must(err)
	must(b.SetRelPropAt(r1, "weight", 0.9))
	_, err = b.AddRel(1, 5, "REL TYPE†")
	must(err)
	r3, err := b.AddRel(100, 0, "LIKES")
	must(err)
	must(b.SetRelPropAt(r3, "tag", "x"))
	return b.Finalize()
}

func TestNQuadsRoundTrip(t *testing.T) {
	g := nqTestGraph(t)
	var first bytes.Buffer
	if err := g.WriteNQuads(&first); err != nil {
		t.Fatal(err)
	}
	got, err := chickpeas.ReadNQuads(first.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := got.WriteNQuads(&second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("write -> read -> write not byte-identical:\nfirst:\n%s\nsecond:\n%s",
			first.String(), second.String())
	}

	if got.NodeCount() != g.NodeCount() {
		t.Fatalf("NodeCount = %d, want %d", got.NodeCount(), g.NodeCount())
	}
	if got.RelCount() != g.RelCount() {
		t.Fatalf("RelCount = %d, want %d", got.RelCount(), g.RelCount())
	}
	if v, ok := got.Version(); !ok || v != "nq-test-v1" {
		t.Fatalf("Version = %q, %v", v, ok)
	}
	for _, want := range []struct {
		node  chickpeas.NodeID
		label string
	}{{0, "Person"}, {0, "Admin"}, {1, "Person"}, {100, "Röle with space"}} {
		if !got.HasLabel(want.node, want.label) {
			t.Errorf("node %d missing label %q", want.node, want.label)
		}
	}
	if s := got.Prop(1, "name").StrOr(""); s != "Bob \"quoted\"\nsecond line" {
		t.Errorf("name(1) = %q", s)
	}
	if v := got.Prop(0, "age").I64Or(0); v != 42 {
		t.Errorf("age(0) = %d", v)
	}
	if v := got.Prop(0, "score").F64Or(0); v != 3.14 {
		t.Errorf("score(0) = %v", v)
	}
	if v := got.Prop(0, "active").BoolOr(false); !v {
		t.Errorf("active(0) = false")
	}
	if s := got.Prop(5, "has space%25").StrOr(""); s != "odd key" {
		t.Errorf("odd key prop = %q", s)
	}
	if v := got.Prop(100, "neg").I64Or(0); v != -7 {
		t.Errorf("neg(100) = %d", v)
	}
	if v := got.Prop(100, "infp").F64Or(0); !math.IsInf(v, 1) {
		t.Errorf("infp(100) = %v", v)
	}
	if v := got.Prop(100, "infn").F64Or(0); !math.IsInf(v, -1) {
		t.Errorf("infn(100) = %v", v)
	}
	if v := got.Prop(100, "nan").F64Or(0); !math.IsNaN(v) {
		t.Errorf("nan(100) = %v", v)
	}

	// The two parallel KNOWS rels stay distinct and keep their own weights.
	var weights []float64
	for r := range got.Rels(0, chickpeas.Outgoing, "KNOWS") {
		if r.Neighbor != 1 {
			t.Errorf("KNOWS neighbor = %d", r.Neighbor)
		}
		weights = append(weights, got.RelProp(r.Pos, "weight").F64Or(0))
	}
	if len(weights) != 2 || weights[0]+weights[1] != 1.4 {
		t.Errorf("parallel KNOWS weights = %v", weights)
	}
	if !got.HasRel(1, chickpeas.Outgoing, "REL TYPE†") {
		t.Error("missing REL TYPE† rel")
	}
	for r := range got.Rels(100, chickpeas.Outgoing, "LIKES") {
		if s := got.RelProp(r.Pos, "tag").StrOr(""); s != "x" {
			t.Errorf("tag = %q", s)
		}
	}
}

func TestNQuadsFileGzip(t *testing.T) {
	g := nqTestGraph(t)
	dir := t.TempDir()

	gzPath := filepath.Join(dir, "graph.nq.gz")
	if err := g.WriteNQuadsFile(gzPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf(".gz output is not gzipped (starts % x)", raw[:min(4, len(raw))])
	}
	got, err := chickpeas.ReadNQuadsFile(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeCount() != g.NodeCount() || got.RelCount() != g.RelCount() {
		t.Fatalf("gz round trip: %d nodes / %d rels, want %d / %d",
			got.NodeCount(), got.RelCount(), g.NodeCount(), g.RelCount())
	}
	// The sniffing byte path handles gz too, not only the file path.
	if _, err := chickpeas.ReadNQuads(raw); err != nil {
		t.Fatal(err)
	}

	plainPath := filepath.Join(dir, "graph.nq")
	if err := g.WriteNQuadsFile(plainPath); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		t.Fatal("plain output is gzipped")
	}
	if got, err = chickpeas.ReadNQuadsFile(plainPath); err != nil {
		t.Fatal(err)
	} else if got.NodeCount() != g.NodeCount() {
		t.Fatalf("plain round trip NodeCount = %d", got.NodeCount())
	}
}

func TestNQuadsGenericImport(t *testing.T) {
	const doc = `<http://ex.org/alice> <http://www.w3.org/1999/02/22-rdf-syntax-ns#type> <http://ex.org/vocab#Person> .
<http://ex.org/alice> <http://ex.org/vocab#name> "Alice" .
<http://ex.org/alice> <http://ex.org/vocab#name> "Duplicate loses" .
<http://ex.org/alice> <http://ex.org/vocab#age> "42"^^<http://www.w3.org/2001/XMLSchema#integer> .
<http://ex.org/alice> <http://ex.org/vocab#knows> <http://ex.org/bob> <http://ex.org/e1> .
<http://ex.org/e1> <http://ex.org/vocab#weight> "0.5"^^<http://www.w3.org/2001/XMLSchema#double> .
_:b0 <http://ex.org/vocab#follows> <http://ex.org/alice> .
`
	g, err := chickpeas.ReadNQuads([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	// alice, bob, and the blank node; e1 anchors the rel instead of
	// becoming a node.
	if g.NodeCount() != 3 {
		t.Fatalf("NodeCount = %d, want 3", g.NodeCount())
	}
	alice, ok := g.NodeWithProperty("uri", "http://ex.org/alice")
	if !ok {
		t.Fatal("alice not found by uri")
	}
	if !g.HasLabel(alice, "Person") {
		t.Error("alice missing Person label")
	}
	if s := g.Prop(alice, "name").StrOr(""); s != "Alice" {
		t.Errorf("first-wins name = %q", s)
	}
	if v := g.Prop(alice, "age").I64Or(0); v != 42 {
		t.Errorf("age = %d", v)
	}
	if _, ok := g.NodeWithProperty("uri", "http://ex.org/e1"); ok {
		t.Error("edge-anchor e1 became a node")
	}
	bob, ok := g.NodeWithProperty("uri", "http://ex.org/bob")
	if !ok {
		t.Fatal("bob not found by uri")
	}
	found := false
	for r := range g.Rels(alice, chickpeas.Outgoing, "knows") {
		found = true
		if r.Neighbor != bob {
			t.Errorf("knows target = %d, want %d", r.Neighbor, bob)
		}
		if w := g.RelProp(r.Pos, "weight").F64Or(0); w != 0.5 {
			t.Errorf("rel weight = %v", w)
		}
	}
	if !found {
		t.Error("knows rel missing")
	}
	if !g.HasRel(alice, chickpeas.Incoming, "follows") {
		t.Error("blank-node follows rel missing")
	}
}

func TestNQuadsMixedNativeAndForeignIDs(t *testing.T) {
	const doc = `<urn:chickpeas:node/1> <http://ex.org/p> <http://ex.org/a> .
<http://ex.org/b> <http://ex.org/p> <urn:chickpeas:node/1> .
`
	g, err := chickpeas.ReadNQuads([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if g.NodeCount() != 3 {
		t.Fatalf("NodeCount = %d, want 3", g.NodeCount())
	}
	// Native id 1 is claimed by spelling; the foreign IRIs take 0 and 2.
	if a, ok := g.NodeWithProperty("uri", "http://ex.org/a"); !ok || a != 0 {
		t.Errorf("a = %d, %v; want 0", a, ok)
	}
	if b, ok := g.NodeWithProperty("uri", "http://ex.org/b"); !ok || b != 2 {
		t.Errorf("b = %d, %v; want 2", b, ok)
	}
	if _, ok := g.NodeWithProperty("uri", "urn:chickpeas:node/1"); ok {
		t.Error("native node carries a uri property")
	}
}

func TestNQuadsSharedNamedGraphIsNotAnEdgeAnchor(t *testing.T) {
	// Two rels share graph g1, so g1 is provenance, not an edge id: its
	// literal statement makes it a node.
	const doc = `<http://ex.org/a> <http://ex.org/p> <http://ex.org/b> <http://ex.org/g1> .
<http://ex.org/b> <http://ex.org/p> <http://ex.org/a> <http://ex.org/g1> .
<http://ex.org/g1> <http://ex.org/note> "prov" .
`
	g, err := chickpeas.ReadNQuads([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if g.NodeCount() != 3 {
		t.Fatalf("NodeCount = %d, want 3", g.NodeCount())
	}
	g1, ok := g.NodeWithProperty("uri", "http://ex.org/g1")
	if !ok {
		t.Fatal("g1 did not become a node")
	}
	if s := g.Prop(g1, "note").StrOr(""); s != "prov" {
		t.Errorf("note = %q", s)
	}
}

func FuzzReadNQuads(f *testing.F) {
	var buf bytes.Buffer
	if err := nqTestGraph(f).WriteNQuads(&buf); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("<http://a> <http://b> <http://c> <http://g> .\n"))
	f.Add([]byte("<http://a> <http://b> \"x\"^^<http://www.w3.org/2001/XMLSchema#integer> .\n"))
	f.Add([]byte{0x1f, 0x8b, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		snap, err := chickpeas.ReadNQuads(data)
		if err == nil && snap == nil {
			t.Fatal("nil snapshot without error")
		}
	})
}
