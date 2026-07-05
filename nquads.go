// N-Quads interchange for Snapshots (task 031): the RDF sibling of the
// RCPG functions in serialize.go. WriteNQuads serializes a snapshot as a
// deterministic N-Quads document under the urn:chickpeas: vocabulary;
// ReadNQuads maps any N-Quads/N-Triples document -- ours or foreign --
// into a Snapshot. Parsing and term escaping are github.com/freeeve/
// libcodex/rdf.
//
// Write vocabulary:
//   - node subject: urn:chickpeas:node/<id> (decimal; sparse ids survive)
//   - label: <node> rdf:type urn:chickpeas:label/<enc(name)>
//   - node prop: <node> urn:chickpeas:prop/<enc(key)> <literal>
//   - rel: <u> urn:chickpeas:rel/<enc(type)> <v> urn:chickpeas:relid/<pos>
//     -- the graph term is the rel's identity, so parallel rels stay
//     distinct statements and rel properties have an anchor
//   - rel prop: <relid> urn:chickpeas:prop/<enc(key)> <literal>
//   - version: <urn:chickpeas:graph> <urn:chickpeas:version> "<v>"
//
// Read mapping (generic, with the chickpeas vocabulary as a special
// spelling of each rule):
//   - rdf:type with an IRI object labels the subject (chickpeas label
//     prefix decoded, otherwise the local name after '#'/'/');
//   - a literal object becomes a property of the subject, first value
//     wins, typed from the xsd datatype;
//   - a resource object becomes a rel typed by the predicate's local
//     or decoded name;
//   - a non-default graph term tagging exactly one rel quad, never used
//     as a rel/label subject or object, anchors that rel: literal
//     statements about it attach as rel properties (the named-graph-
//     per-edge pattern);
//   - urn:chickpeas:node/<n> terms keep id n; other entities intern
//     sequentially in document order, and foreign IRI entities carry a
//     `uri` property (blank nodes don't).
//
// A bare node (no labels, props, or rels) appears in no quad and does
// not survive a write/read cycle; RCPG remains the fidelity format.

package chickpeas

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/freeeve/gochickpeas/nodeset"
	"github.com/freeeve/libcodex/rdf"
)

const (
	nqNodePrefix  = "urn:chickpeas:node/"
	nqLabelPrefix = "urn:chickpeas:label/"
	nqPropPrefix  = "urn:chickpeas:prop/"
	nqRelPrefix   = "urn:chickpeas:rel/"
	nqRelIDPrefix = "urn:chickpeas:relid/"
	nqGraphIRI    = "urn:chickpeas:graph"
	nqVersionIRI  = "urn:chickpeas:version"

	xsdInteger = "http://www.w3.org/2001/XMLSchema#integer"
	xsdDouble  = "http://www.w3.org/2001/XMLSchema#double"
	xsdBoolean = "http://www.w3.org/2001/XMLSchema#boolean"

	nqFlushSize = 1 << 20
)

// WriteNQuads serializes the snapshot as an N-Quads document (see the
// package-level vocabulary above). Output order is deterministic --
// version, then nodes in ascending id order (labels sorted, property
// keys sorted, outgoing rels in CSR order, each followed by its sorted
// rel properties) -- so equal graphs serialize byte-identically.
func (g *Snapshot) WriteNQuads(w io.Writer) error {
	var enc rdf.Encoder
	buf := make([]byte, 0, nqFlushSize+(nqFlushSize>>2))
	flush := func() error {
		_, err := w.Write(buf)
		buf = buf[:0]
		return err
	}

	if v, ok := g.Version(); ok {
		buf = enc.AppendQuad(buf, rdf.Quad{
			S: rdf.NewIRI(nqGraphIRI),
			P: rdf.NewIRI(nqVersionIRI),
			O: rdf.NewLiteral(v, "", ""),
		})
	}

	labelNames := g.Labels()
	labelSets := make([]*nodeset.Set, len(labelNames))
	labelTerms := make([]rdf.Term, len(labelNames))
	for i, name := range labelNames {
		labelSets[i], _ = g.NodesWithLabel(name)
		labelTerms[i] = rdf.NewIRI(nqLabelPrefix + nqEscape(name))
	}

	// Per-relKey invariants precompute once: the predicate IRI (an
	// allocating concat+escape) and the column lookup would otherwise
	// recompute per (rel x relKey) in the inner loop -- M*K times for K
	// distinct values.
	type relColKey struct {
		name string
		pred rdf.Term
		col  Column
	}
	relKeys := make([]relColKey, 0, len(g.relColumns))
	for k, col := range g.relColumns {
		if name, ok := g.atoms.Resolve(k); ok {
			relKeys = append(relKeys, relColKey{
				name: name,
				pred: rdf.NewIRI(nqPropPrefix + nqEscape(name)),
				col:  col,
			})
		}
	}
	sort.Slice(relKeys, func(i, j int) bool { return relKeys[i].name < relKeys[j].name })

	// The rel-type predicate is likewise invariant per type atom: resolve
	// and escape each once instead of per rel.
	typeTerms := make(map[RelType]rdf.Term, len(g.typeIndex))
	for t := range g.typeIndex {
		if name, ok := g.atoms.Resolve(t.ID()); ok {
			typeTerms[t] = rdf.NewIRI(nqRelPrefix + nqEscape(name))
		}
	}

	n := int(g.CSRIDSpace())
	for u := range n {
		id := NodeID(u)
		subj := rdf.NewIRI(nqNodePrefix + strconv.FormatUint(uint64(u), 10))
		for i, set := range labelSets {
			if set.Contains(id) {
				buf = enc.AppendQuad(buf, rdf.Quad{
					S: subj,
					P: rdf.NewIRI(rdf.TypeIRI),
					O: labelTerms[i],
				})
			}
		}
		for _, key := range g.NodePropertyKeys(id) {
			if v, ok := g.Prop(id, key).Value(); ok {
				buf = enc.AppendQuad(buf, rdf.Quad{
					S: subj,
					P: rdf.NewIRI(nqPropPrefix + nqEscape(key)),
					O: g.nqLiteral(v),
				})
			}
		}
		for pos := g.outOffsets[u]; pos < g.outOffsets[u+1]; pos++ {
			relTerm := rdf.NewIRI(nqRelIDPrefix + strconv.FormatUint(uint64(pos), 10))
			buf = enc.AppendQuad(buf, rdf.Quad{
				S: subj,
				P: typeTerms[g.outTypes[pos]],
				O: rdf.NewIRI(nqNodePrefix + strconv.FormatUint(uint64(g.outNbrs[pos]), 10)),
				G: relTerm,
			})
			for _, rk := range relKeys {
				if v, ok := rk.col.Get(pos); ok {
					buf = enc.AppendQuad(buf, rdf.Quad{
						S: relTerm,
						P: rk.pred,
						O: g.nqLiteral(v),
					})
				}
			}
		}
		if len(buf) >= nqFlushSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// WriteNQuadsFile writes the snapshot as an N-Quads file, gzip-compressed
// when path ends in ".gz". Any other compression wraps WriteNQuads in a
// caller-provided writer.
func (g *Snapshot) WriteNQuadsFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var w io.Writer = f
	var gz *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		gz = gzip.NewWriter(f)
		w = gz
	}
	if err := g.WriteNQuads(w); err != nil {
		f.Close()
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// nqLiteral maps a property Value to a typed RDF literal.
func (g *Snapshot) nqLiteral(v Value) rdf.Term {
	switch v.Kind() {
	case KindI64:
		x, _ := v.I64()
		return rdf.NewLiteral(strconv.FormatInt(x, 10), "", xsdInteger)
	case KindF64:
		x, _ := v.F64()
		return rdf.NewLiteral(nqFormatFloat(x), "", xsdDouble)
	case KindBool:
		x, _ := v.Bool()
		return rdf.NewLiteral(strconv.FormatBool(x), "", xsdBoolean)
	}
	atom, _ := v.StrID()
	s, _ := g.atoms.Resolve(atom)
	return rdf.NewLiteral(s, "", "")
}

// nqFormatFloat writes the shortest round-tripping decimal form, using
// the xsd:double lexicals for the non-finite values.
func nqFormatFloat(x float64) string {
	switch {
	case math.IsInf(x, 1):
		return "INF"
	case math.IsInf(x, -1):
		return "-INF"
	case math.IsNaN(x):
		return "NaN"
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// nqEscape percent-encodes the bytes an IRI cannot carry raw (control
// characters, space, the N-Triples delimiters, and '%' itself), leaving
// multi-byte UTF-8 intact. nqUnescape inverts it.
func nqEscape(s string) string {
	needsEscape := func(c byte) bool {
		return c <= ' ' || c == 0x7f || strings.IndexByte("<>\"{}|^`\\%", c) >= 0
	}
	i := 0
	for i < len(s) && !needsEscape(s[i]) {
		i++
	}
	if i == len(s) {
		return s
	}
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s)+8)
	out = append(out, s[:i]...)
	for ; i < len(s); i++ {
		if c := s[i]; needsEscape(c) {
			out = append(out, '%', hex[c>>4], hex[c&0xf])
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// nqUnescape decodes %XX sequences; malformed sequences pass through
// verbatim.
func nqUnescape(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := nqHexVal(s[i+1]), nqHexVal(s[i+2]); hi >= 0 && lo >= 0 {
				out = append(out, byte(hi<<4|lo))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

func nqHexVal(c byte) int {
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

// ReadNQuadsFile reads an N-Quads/N-Triples file (transparently
// gunzipping) into a Snapshot via ReadNQuads.
func ReadNQuadsFile(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadNQuads(data)
}

// ReadNQuads builds a Snapshot from an N-Quads or N-Triples document
// using the read mapping documented at the top of this file. Gzipped
// input (the 1f 8b magic) is decompressed transparently.
func ReadNQuads(data []byte) (*Snapshot, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		unzipped, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		if err := zr.Close(); err != nil {
			return nil, err
		}
		data = unzipped
	}
	ds, err := rdf.ParseNQuadsShared(data)
	if err != nil {
		return nil, err
	}
	return buildFromQuads(ds.Quads)
}

// Quad classification for buildFromQuads.
const (
	nqQSkip uint8 = iota
	nqQLabel
	nqQProp
	nqQRel
)

// buildFromQuads maps parsed quads into a Snapshot per the read mapping.
func buildFromQuads(quads []rdf.Quad) (*Snapshot, error) {
	// Pass 1: classify each quad, count rel quads per named graph term,
	// and note which terms act as rel/label subjects or objects (those
	// can never anchor rel properties).
	kind := make([]uint8, len(quads))
	relQuadsPer := map[rdf.Term]int{}
	entityUse := map[rdf.Term]bool{}
	var version string
	var hasVersion bool
	for i := range quads {
		q := &quads[i]
		switch {
		case q.S.Kind == rdf.IRI && q.S.Value == nqGraphIRI &&
			q.P.Value == nqVersionIRI && q.O.Kind == rdf.Literal:
			kind[i] = nqQSkip
			version, hasVersion = q.O.Value, true
		case q.P.Value == rdf.TypeIRI && q.O.Kind == rdf.IRI:
			kind[i] = nqQLabel
			entityUse[q.S] = true
		case q.O.Kind == rdf.Literal:
			kind[i] = nqQProp
		default:
			kind[i] = nqQRel
			entityUse[q.S] = true
			entityUse[q.O] = true
			if q.G != (rdf.Term{}) {
				relQuadsPer[q.G]++
			}
		}
	}
	anchor := map[rdf.Term]bool{}
	for gterm, cnt := range relQuadsPer {
		if cnt == 1 && !entityUse[gterm] {
			anchor[gterm] = true
		}
	}

	// Pass 2: assign node ids. Native urn:chickpeas:node/<n> terms claim
	// their id first (spelling decides, independent of position); the
	// rest intern sequentially in document order, skipping claimed ids.
	ids := map[rdf.Term]NodeID{}
	used := map[NodeID]bool{}
	forEachEntity := func(visit func(t rdf.Term)) {
		for i := range quads {
			q := &quads[i]
			switch kind[i] {
			case nqQLabel:
				visit(q.S)
			case nqQProp:
				if !anchor[q.S] {
					visit(q.S)
				}
			case nqQRel:
				visit(q.S)
				visit(q.O)
			}
		}
	}
	forEachEntity(func(t rdf.Term) {
		if _, seen := ids[t]; seen {
			return
		}
		if id, ok := nqNativeID(t); ok {
			ids[t] = id
			used[id] = true
		}
	})
	var next NodeID
	forEachEntity(func(t rdf.Term) {
		if _, seen := ids[t]; seen {
			return
		}
		for used[next] {
			next++
		}
		ids[t] = next
		used[next] = true
	})

	// Pass 3: collect labels, then create nodes in ascending id order.
	// Foreign IRI entities carry their IRI as a `uri` property.
	labels := map[NodeID]map[string]bool{}
	for i := range quads {
		if kind[i] != nqQLabel {
			continue
		}
		q := &quads[i]
		id := ids[q.S]
		if labels[id] == nil {
			labels[id] = map[string]bool{}
		}
		labels[id][nqDecodeName(q.O.Value, nqLabelPrefix)] = true
	}
	uriOf := map[NodeID]string{}
	for t, id := range ids {
		if t.Kind == rdf.IRI {
			if _, native := nqNativeID(t); !native {
				uriOf[id] = t.Value
			}
		}
	}
	order := make([]NodeID, 0, len(used))
	for id := range used {
		order = append(order, id)
	}
	slices.Sort(order)
	b := NewBuilder(len(used), len(quads))
	seenProp := map[uint64]bool{}
	uriKey := b.InternPropertyKey("uri")
	for _, id := range order {
		names := make([]string, 0, len(labels[id]))
		for name := range labels[id] {
			names = append(names, name)
		}
		sort.Strings(names)
		if _, err := b.AddNodeWithID(id, names...); err != nil {
			return nil, err
		}
		if uri, ok := uriOf[id]; ok {
			if err := b.SetPropByKey(id, uriKey, uri); err != nil {
				return nil, err
			}
			seenProp[uint64(id)<<32|uint64(uriKey)] = true
		}
	}

	// Pass 4: rels in document order, remembering which builder index
	// each anchoring graph term produced.
	relIdxOf := map[rdf.Term]int{}
	for i := range quads {
		if kind[i] != nqQRel {
			continue
		}
		q := &quads[i]
		idx, err := b.AddRel(ids[q.S], ids[q.O], nqDecodeName(q.P.Value, nqRelPrefix))
		if err != nil {
			return nil, err
		}
		if anchor[q.G] {
			relIdxOf[q.G] = idx
		}
	}

	// Pass 5: properties in document order, first value wins per
	// (entity, key); anchor subjects address their rel instead of a node.
	seenRelProp := map[uint64]bool{}
	for i := range quads {
		if kind[i] != nqQProp {
			continue
		}
		q := &quads[i]
		key := b.InternPropertyKey(nqDecodeName(q.P.Value, nqPropPrefix))
		val := nqLiteralValue(q.O)
		if anchor[q.S] {
			idx := relIdxOf[q.S]
			guard := uint64(idx)<<32 | uint64(key)
			if seenRelProp[guard] {
				continue
			}
			seenRelProp[guard] = true
			if err := b.SetRelPropAt(idx, nqDecodeName(q.P.Value, nqPropPrefix), val); err != nil {
				return nil, err
			}
			continue
		}
		id := ids[q.S]
		guard := uint64(id)<<32 | uint64(key)
		if seenProp[guard] {
			continue
		}
		seenProp[guard] = true
		if err := b.SetPropByKey(id, key, val); err != nil {
			return nil, err
		}
	}

	if hasVersion {
		b.SetVersion(version)
	}
	return b.Finalize(), nil
}

// nqNativeID matches urn:chickpeas:node/<decimal> and returns the id.
func nqNativeID(t rdf.Term) (NodeID, bool) {
	if t.Kind != rdf.IRI || !strings.HasPrefix(t.Value, nqNodePrefix) {
		return 0, false
	}
	n, err := strconv.ParseUint(t.Value[len(nqNodePrefix):], 10, 32)
	if err != nil {
		return 0, false
	}
	return NodeID(n), true
}

// nqDecodeName maps an IRI to a label/key/type name: the unescaped
// remainder for the given chickpeas prefix, the local name otherwise.
func nqDecodeName(iri, prefix string) string {
	if strings.HasPrefix(iri, prefix) {
		return nqUnescape(iri[len(prefix):])
	}
	return nqLocalName(iri)
}

// nqLocalName is the substring after the last '#', then after the last
// '/' of that remainder (the SPB loader's local-name rule).
func nqLocalName(iri string) string {
	if i := strings.LastIndexByte(iri, '#'); i >= 0 {
		iri = iri[i+1:]
	}
	if i := strings.LastIndexByte(iri, '/'); i >= 0 {
		iri = iri[i+1:]
	}
	return iri
}

// nqLiteralValue maps a literal to the most specific property type its
// xsd datatype allows, falling back to the lexical string.
func nqLiteralValue(t rdf.Term) any {
	switch nqLocalName(t.Datatype) {
	case "integer", "int", "long", "short", "byte",
		"nonNegativeInteger", "positiveInteger",
		"nonPositiveInteger", "negativeInteger",
		"unsignedLong", "unsignedInt", "unsignedShort", "unsignedByte":
		if v, err := strconv.ParseInt(t.Value, 10, 64); err == nil {
			return v
		}
	case "double", "float", "decimal":
		switch t.Value {
		case "INF", "+INF":
			return math.Inf(1)
		case "-INF":
			return math.Inf(-1)
		case "NaN":
			return math.NaN()
		}
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
