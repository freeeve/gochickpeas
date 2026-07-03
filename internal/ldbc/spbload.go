// SPB RDF -> property-graph loader (task 026): maps N-Triples/N-Quads
// into a Snapshot with the same convention as rustychickpeas-ldbc
// src/spb/loader.rs, so the two engines run the SPB kernels over an
// identical graph:
//   - every IRI/blank subject or IRI object becomes a node (one per
//     resource);
//   - rdf:type makes the object's local name a label on the subject,
//     expanded with the rdfs:subClassOf transitive closure (owl:Thing
//     dropped);
//   - a predicate with a resource object becomes a typed rel (local
//     name of the predicate), re-materialized for each rdfs:subPropertyOf
//     super-property (an `about`/`mentions` rel is also a `tag` rel);
//   - a predicate with a literal object becomes a node property (local
//     name), typed from the literal's xsd: datatype, first value wins;
//   - every IRI node carries a `uri` property (the cross-engine key),
//     percent-decoded so encoded and raw spellings intern to one node.
//
// Parsing is github.com/freeeve/libcodex/rdf, which already unescapes
// UCHAR (\uXXXX/\UXXXXXXXX) sequences in IRIs; percent-decoding is the
// one canonicalization added here, mirroring the Rust loader.

package ldbc

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/libcodex/rdf"
)

const (
	rdfTypeIRI      = "http://www.w3.org/1999/02/22-rdf-syntax-ns#type"
	rdfsSubClassIRI = "http://www.w3.org/2000/01/rdf-schema#subClassOf"
	rdfsSubPropIRI  = "http://www.w3.org/2000/01/rdf-schema#subPropertyOf"
	// The trivial universal class: never materialized (no query targets
	// it, and its local name collides with coreconcepts:Thing).
	owlThingIRI = "http://www.w3.org/2002/07/owl#Thing"
)

// SPBStats counts one load, for the export banner and the cross-check
// against the Rust loader's banner (both parsers skip malformed lines
// silently, so equal counts are the load-level parity signal).
type SPBStats struct {
	Resources int
	Triples   int
	Rels      int
	Literals  int
}

// LoadSPBFile reads an N-Triples/N-Quads file (any fourth term ignored)
// and builds the SPB property graph.
func LoadSPBFile(path string) (*chickpeas.Snapshot, SPBStats, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, SPBStats{}, err
	}
	g, err := rdf.ParseNTriplesShared(data)
	if err != nil {
		return nil, SPBStats{}, err
	}
	snap, stats := BuildSPBGraph(g.Triples)
	return snap, stats, nil
}

// LoadSPBString builds the SPB graph from an N-Triples document in
// memory (the testable core of LoadSPBFile).
func LoadSPBString(doc string) (*chickpeas.Snapshot, SPBStats) {
	g, _ := rdf.ParseNTriples([]byte(doc))
	return BuildSPBGraph(g.Triples)
}

// BuildSPBGraph maps parsed triples into a Snapshot per the loader
// convention above.
func BuildSPBGraph(triples []rdf.Triple) (*chickpeas.Snapshot, SPBStats) {
	subclass, subprop := spbTBox(triples)

	// Pass 1: assign a node id to every resource in document order,
	// collect each subject's rdf:type IRIs, and remember each IRI to
	// store as the `uri` property. TBox triples are not instance data,
	// and an rdf:type object names a class, not a node.
	ids := map[string]chickpeas.NodeID{}
	types := map[chickpeas.NodeID][]string{}
	uriOf := map[chickpeas.NodeID]string{}
	var next chickpeas.NodeID
	intern := func(t rdf.Term) chickpeas.NodeID {
		key := spbResourceKey(t)
		if id, ok := ids[key]; ok {
			return id
		}
		id := next
		next++
		ids[key] = id
		if t.Kind == rdf.IRI {
			uriOf[id] = key[2:] // the percent-decoded IRI behind the "I:" prefix
		}
		return id
	}
	for i := range triples {
		t := &triples[i]
		if spbIsTBox(t) {
			continue
		}
		sid := intern(t.S)
		if t.P.Value == rdfTypeIRI {
			if t.O.Kind == rdf.IRI {
				types[sid] = append(types[sid], t.O.Value)
			}
		} else if t.O.Kind != rdf.Literal {
			intern(t.O)
		}
	}

	// Pass 2: create nodes in id order with their labels -- each type's
	// local name plus those of its transitive super-classes, minus
	// owl:Thing, deduplicated and sorted for a deterministic build.
	b := chickpeas.NewBuilder(int(next), len(triples))
	for id := chickpeas.NodeID(0); id < next; id++ {
		labelSet := map[string]bool{}
		for _, ty := range types[id] {
			labelSet[spbLocalName(ty)] = true
			for super := range subclass[ty] {
				if super != owlThingIRI {
					labelSet[spbLocalName(super)] = true
				}
			}
		}
		labels := make([]string, 0, len(labelSet))
		for l := range labelSet {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		if _, err := b.AddNodeWithID(id, labels...); err != nil {
			panic("spbload: add node: " + err.Error())
		}
		if uri, ok := uriOf[id]; ok {
			if err := b.SetProp(id, "uri", uri); err != nil {
				panic("spbload: set uri: " + err.Error())
			}
		}
	}

	// Pass 3: rels (resource objects, re-materialized per super-property
	// in sorted order for determinism) and first-wins literal properties.
	stats := SPBStats{Resources: int(next), Triples: len(triples)}
	seenProp := map[uint64]bool{}
	for i := range triples {
		t := &triples[i]
		if t.P.Value == rdfTypeIRI || spbIsTBox(t) {
			continue
		}
		subj := ids[spbResourceKey(t.S)]
		key := spbLocalName(t.P.Value)
		if t.O.Kind != rdf.Literal {
			dst := ids[spbResourceKey(t.O)]
			if _, err := b.AddRel(subj, dst, key); err != nil {
				panic("spbload: add rel: " + err.Error())
			}
			stats.Rels++
			supers := make([]string, 0, len(subprop[t.P.Value]))
			for s := range subprop[t.P.Value] {
				supers = append(supers, s)
			}
			sort.Strings(supers)
			for _, s := range supers {
				if _, err := b.AddRel(subj, dst, spbLocalName(s)); err != nil {
					panic("spbload: add super rel: " + err.Error())
				}
			}
			continue
		}
		pk := b.InternPropertyKey(key)
		guard := uint64(subj)<<32 | uint64(pk)
		if seenProp[guard] {
			continue
		}
		seenProp[guard] = true
		if err := b.SetPropByKey(subj, pk, spbLiteralValue(t.O)); err != nil {
			panic("spbload: set prop: " + err.Error())
		}
		stats.Literals++
	}

	return b.Finalize(), stats
}

// spbTBox collects rdfs:subClassOf / rdfs:subPropertyOf over IRI terms
// and closes both maps under transitivity, so each type/predicate maps
// to all its ancestors. Inputs are tiny (an ontology TBox).
func spbTBox(triples []rdf.Triple) (subclass, subprop map[string]map[string]bool) {
	subclass = map[string]map[string]bool{}
	subprop = map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, k, v string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][v] = true
	}
	for i := range triples {
		t := &triples[i]
		if t.S.Kind != rdf.IRI || t.P.Kind != rdf.IRI || t.O.Kind != rdf.IRI {
			continue
		}
		switch t.P.Value {
		case rdfsSubClassIRI:
			add(subclass, t.S.Value, t.O.Value)
		case rdfsSubPropIRI:
			add(subprop, t.S.Value, t.O.Value)
		}
	}
	spbCloseTransitively(subclass)
	spbCloseTransitively(subprop)
	return subclass, subprop
}

// spbCloseTransitively closes a direct super-of map under transitivity.
func spbCloseTransitively(m map[string]map[string]bool) {
	for changed := true; changed; {
		changed = false
		for k := range m {
			for d := range m[k] {
				for s := range m[d] {
					if !m[k][s] {
						m[k][s] = true
						changed = true
					}
				}
			}
		}
	}
}

// spbIsTBox reports whether a triple is an RDFS TBox statement, consumed
// for forward-chaining rather than loaded as instance data.
func spbIsTBox(t *rdf.Triple) bool {
	return t.P.Value == rdfsSubClassIRI || t.P.Value == rdfsSubPropIRI
}

// spbResourceKey is the interning identity of a resource term,
// namespaced so a blank `x` and an IRI `x` never collide; IRIs are
// percent-decoded so encoded and raw spellings resolve to one node.
func spbResourceKey(t rdf.Term) string {
	if t.Kind == rdf.Blank {
		return "B:" + t.Value
	}
	return "I:" + spbPercentDecode(t.Value)
}

// spbPercentDecode decodes %XX sequences to a canonical IRI form,
// matching how an RDF store canonicalizes on load. Invalid hex or a
// decode that is not valid UTF-8 leaves the input verbatim.
func spbPercentDecode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := hexVal(s[i+1]), hexVal(s[i+2]); hi >= 0 && lo >= 0 {
				out = append(out, byte(hi*16+lo))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
		i++
	}
	if !utf8.Valid(out) {
		return s
	}
	return string(out)
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// spbLocalName is the local name of an IRI exactly as the Rust loader
// computes it: the substring after the last '#', then after the last
// '/' of that remainder.
func spbLocalName(iri string) string {
	if i := strings.LastIndexByte(iri, '#'); i >= 0 {
		iri = iri[i+1:]
	}
	if i := strings.LastIndexByte(iri, '/'); i >= 0 {
		iri = iri[i+1:]
	}
	return iri
}

// spbLiteralValue maps a literal to the most specific property type its
// xsd: datatype allows, falling back to the lexical string. Boolean
// accepts exactly true/false, mirroring Rust's bool parse.
func spbLiteralValue(t rdf.Term) any {
	switch spbLocalName(t.Datatype) {
	case "integer", "int", "long", "short", "byte", "nonNegativeInteger", "positiveInteger":
		if v, err := strconv.ParseInt(t.Value, 10, 64); err == nil {
			return v
		}
	case "double", "float", "decimal":
		if v, err := strconv.ParseFloat(t.Value, 64); err == nil {
			return v
		}
	case "boolean":
		switch t.Value {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return t.Value
}
