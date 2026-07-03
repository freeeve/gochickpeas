// Package compile lowers a bound expression into a CExpr whose leaves
// carry resolved row slots and typed Snapshot column readers, so per-row
// evaluation does no string hashing (the var->slot and key->column lookups
// happen once at stage compile). Evaluation mirrors the interpreter
// exactly; nodes not worth specializing fall back to it. Port of the Rust
// compile.rs -- the one package besides the graph seam that touches the
// engine directly.
package compile

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// typedColKind discriminates a typedCol.
type typedColKind uint8

const (
	colI64 typedColKind = iota
	colF64
	colBool
	colStr
	colKeyed
)

// typedCol is a column reader resolved once (the key->column lookup
// hoisted out of the row loop). A missing column falls back to the keyed
// per-row read.
type typedCol struct {
	kind typedColKind
	i64  chickpeas.I64Col
	f64  chickpeas.F64Col
	bool chickpeas.BoolCol
	str  chickpeas.StrCol
}

// newTypedCol narrows a resolved column by dtype.
func newTypedCol(col chickpeas.Col, ok bool) typedCol {
	if !ok {
		return typedCol{kind: colKeyed}
	}
	switch col.Dtype() {
	case chickpeas.DtypeI64:
		return typedCol{kind: colI64, i64: col.I64()}
	case chickpeas.DtypeF64:
		return typedCol{kind: colF64, f64: col.F64()}
	case chickpeas.DtypeBool:
		return typedCol{kind: colBool, bool: col.Bool()}
	default:
		return typedCol{kind: colStr, str: col.Str()}
	}
}

// propReader is a resolved reader for a property key on whichever a slot
// holds -- a node or a relationship -- both typed columns hoisted at
// compile time.
type propReader struct {
	g    *chickpeas.Snapshot
	key  string
	node typedCol
	rel  typedCol
}

// newPropReader hoists the node and rel columns for key once.
func newPropReader(g *chickpeas.Snapshot, key string) propReader {
	nc, nok := g.ColIndexed(key)
	rc, rok := g.RelColIndexed(key)
	return propReader{g: g, key: key, node: newTypedCol(nc, nok), rel: newTypedCol(rc, rok)}
}

// readNode reads the node column at id; absent (including the engine's
// empty-string-means-missing convention) is Null, identical to the seam's
// NodeProp.
func (r *propReader) readNode(id chickpeas.NodeID) value.Value {
	return r.read(&r.node, uint32(id), true)
}

// readRel reads the rel column at a CSR position.
func (r *propReader) readRel(pos uint32) value.Value {
	return r.read(&r.rel, pos, false)
}

// read reads one typed column position, folding absents to Null.
func (r *propReader) read(c *typedCol, pos uint32, isNode bool) value.Value {
	switch c.kind {
	case colI64:
		if v, ok := c.i64.Get(pos); ok {
			return value.Int(v)
		}
	case colF64:
		if v, ok := c.f64.Get(pos); ok {
			return value.Float(v)
		}
	case colBool:
		if v, ok := c.bool.Get(pos); ok {
			return value.Bool(v)
		}
	case colStr:
		// Atom 0 in a dense column means missing (the raw reader does not
		// fold it); an empty or unresolvable text is also absent, matching
		// the engine's Prop convention.
		if id, ok := c.str.ID(pos); ok && id != 0 {
			if s, found := r.g.ResolveString(id); found && s != "" {
				return value.Str(s)
			}
		}
	default:
		// No resolved column: the keyed per-row fallback.
		var p chickpeas.Prop
		if isNode {
			p = r.g.Prop(chickpeas.NodeID(pos), r.key)
		} else {
			p = r.g.RelProp(pos, r.key)
		}
		return propValue(p)
	}
	return value.Null()
}

// propValue converts an engine property read to a runtime value (the same
// folding as the graph seam's NodeProp).
func propValue(p chickpeas.Prop) value.Value {
	v, ok := p.Value()
	if !ok {
		return value.Value{}
	}
	switch v.Kind() {
	case chickpeas.KindI64:
		i, _ := v.I64()
		return value.Int(i)
	case chickpeas.KindF64:
		f, _ := v.F64()
		return value.Float(f)
	case chickpeas.KindBool:
		b, _ := v.Bool()
		return value.Bool(b)
	default:
		if s, ok := p.Str(); ok {
			return value.Str(s)
		}
		return value.Value{}
	}
}
