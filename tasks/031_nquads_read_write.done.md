# 031: N-Quads read/write for Snapshots

Add a public N-Quads interchange surface to the root `chickpeas` package,
alongside the RCPG functions in serialize.go. Parsing/serialization is
`github.com/freeeve/libcodex/rdf` (already a dependency since 026).

## API (nquads.go)

- `(g *Snapshot) WriteNQuads(w io.Writer) error`
- `(g *Snapshot) WriteNQuadsFile(path string) error` -- gzips when the
  path ends in `.gz` (the opt-in flag is the extension; any other
  compression goes through `WriteNQuads` with a caller-wrapped writer)
- `ReadNQuads(data []byte) (*Snapshot, error)` -- transparent gzip
  (sniffs the 1f 8b magic), accepts N-Quads or N-Triples
- `ReadNQuadsFile(path string) (*Snapshot, error)`

## Write vocabulary (`urn:chickpeas:`)

- node subject: `urn:chickpeas:node/<id>` (decimal, preserves sparse ids)
- label: `<node> rdf:type urn:chickpeas:label/<enc(name)>`
- node prop: `<node> urn:chickpeas:prop/<enc(key)> <literal>`
- rel: `<u> urn:chickpeas:rel/<enc(type)> <v> urn:chickpeas:relid/<pos>`
  -- the graph term is the rel's identity, so parallel rels stay
  distinct statements and rel properties have an anchor
- rel prop: `<relid> urn:chickpeas:prop/<enc(key)> <literal>` (default graph)
- version: `<urn:chickpeas:graph> <urn:chickpeas:version> "<v>"`
- literals typed xsd:integer / xsd:double (INF/-INF/NaN lexicals) /
  xsd:boolean / plain string; `<enc()>` percent-encodes bytes an IRI
  cannot carry raw
- deterministic order: version, then nodes ascending (labels sorted,
  keys sorted, out-rels in CSR order with their props), so equal graphs
  serialize byte-identically

## Read mapping (foreign docs and our own)

- `rdf:type` + IRI object -> label (chickpeas prefix decoded, else
  local name after `#`/`/`)
- literal object -> property, first value wins; typed from the xsd
  datatype like the SPB loader
- resource object -> rel (predicate local name / decoded chickpeas type)
- a non-default graph term that tags exactly one rel quad and is never
  itself a rel/label subject or object anchors that rel: literal
  statements about it become rel properties (the named-graph-per-edge
  pattern; our writer's relid terms hit this rule)
- `urn:chickpeas:node/<n>` subjects/objects keep id n; other entities
  get sequential ids in document order, skipping claimed ids; foreign
  IRI entities carry a `uri` property (SPB convention), blanks don't

## Known lossiness

- a bare node (no labels, props, or rels) appears in no quad, so it does
  not survive a write/read cycle (rcpg remains the fidelity format)
- a node's `uri` property round-trips as a property; it is not promoted
  back to the subject IRI

## Done when

- round trip write -> read -> write is byte-identical on a graph with
  sparse ids, parallel rels, rel props, all four value kinds, float
  specials, unicode/space/percent names, and a version string
- .gz write flag honored, gz reads transparent, foreign-doc import
  covered by tests, fuzz on the reader, gofmt -s / vet / race clean
