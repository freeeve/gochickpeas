// N-Quads serialization: writing a snapshot as a deterministic, byte-stable
// N-Quads document (optionally gzip-compressed), the property Value -> RDF
// literal mapping, and IRI percent-escaping. Split from nquads.go, which
// holds the vocabulary, constants, and the parse/build (read) path.
package chickpeas

import (
	"compress/gzip"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/freeeve/gochickpeas/nodeset"
	"github.com/freeeve/libcodex/rdf"
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
