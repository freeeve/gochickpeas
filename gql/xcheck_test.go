// The cross-engine golden-corpus harness: every record's gql query runs
// over its named fixture graph (loaded through the RCPG codec, both eval
// paths) and the produced rows must equal the recorded rows under the
// canonical JSON value encoding documented in testdata/xcheck/README.md.
// The Rust engine exports the authoritative corpus (rustychickpeas
// tasks/200); the seed/ corpus is Go-generated to pin the schema, with
// GOCHICKPEAS_XCHECK_REGEN=1 regenerating it.
package gql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// xcheckRecord is one corpus entry (see testdata/xcheck/README.md).
type xcheckRecord struct {
	Cypher    *string                    `json:"cypher"`
	GQL       *string                    `json:"gql"`
	Graph     string                     `json:"graph"`
	Params    map[string]json.RawMessage `json:"params,omitempty"`
	Unordered bool                       `json:"unordered,omitempty"`
	Columns   []string                   `json:"columns"`
	Rows      []json.RawMessage          `json:"rows"`
}

// weightedGraph is a fixture where the cheapest path is not the fewest-hop
// path: s -0.5-> a -0.5-> t versus the direct s -5.0-> t edge.
func weightedGraph(t testing.TB) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	for _, n := range []string{"s", "a", "t"} {
		id, _ := b.AddNode("N")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range []struct {
		u, v chickpeas.NodeID
		w    float64
	}{{0, 1, 0.5}, {1, 2, 0.5}, {0, 2, 5.0}} {
		if _, err := b.AddRel(e.u, e.v, "E"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelProp(e.u, e.v, "E", "w", e.w); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

// xcheckBuilders are the harness's built-in seed graphs, keyed by the
// corpus record's graph name (geoGraph is the M19 CALL-test fixture).
var xcheckBuilders = map[string]func(testing.TB) *chickpeas.Snapshot{
	"social":   func(t testing.TB) *chickpeas.Snapshot { return socialGraph(t) },
	"replies":  func(t testing.TB) *chickpeas.Snapshot { return replyForest(t.(*testing.T)) },
	"weighted": weightedGraph,
	"geo":      func(t testing.TB) *chickpeas.Snapshot { return geoGraph(t.(*testing.T)) },
}

// encodeValue maps a runtime value to the canonical corpus encoding.
func encodeValue(v value.Value) any {
	switch v.Kind() {
	case value.KindNull:
		return nil
	case value.KindBool:
		b, _ := v.AsBool()
		return b
	case value.KindInt:
		i, _ := v.AsInt()
		return json.Number(strconv.FormatInt(i, 10))
	case value.KindFloat:
		f, _ := v.AsFloat()
		return map[string]any{"float": strconv.FormatFloat(f, 'g', -1, 64)}
	case value.KindStr:
		s, _ := v.AsStr()
		return s
	case value.KindNode:
		n, _ := v.AsNode()
		return map[string]any{"node": json.Number(strconv.FormatUint(uint64(n), 10))}
	case value.KindRel:
		p, _ := v.AsRel()
		return map[string]any{"rel": json.Number(strconv.FormatUint(uint64(p), 10))}
	case value.KindTemporal:
		ms, k, _ := v.AsTemporal()
		return map[string]any{
			"temporal": json.Number(strconv.FormatInt(ms, 10)),
			"kind":     json.Number(strconv.Itoa(int(k))),
		}
	case value.KindDuration:
		mo, d, ms, _ := v.AsDuration()
		return map[string]any{"duration": []any{
			json.Number(strconv.FormatInt(mo, 10)),
			json.Number(strconv.FormatInt(d, 10)),
			json.Number(strconv.FormatInt(ms, 10)),
		}}
	case value.KindList:
		xs, _ := v.AsList()
		out := make([]any, len(xs))
		for i, x := range xs {
			out[i] = encodeValue(x)
		}
		return out
	case value.KindMap:
		entries, _ := v.AsMap()
		enc := make([][2]any, len(entries))
		for i, e := range entries {
			enc[i] = [2]any{e.Key, encodeValue(e.Val)}
		}
		slices.SortFunc(enc, func(a, b [2]any) int {
			return strings.Compare(a[0].(string), b[0].(string))
		})
		out := make([]any, len(enc))
		for i, e := range enc {
			out[i] = []any{e[0], e[1]}
		}
		return map[string]any{"map": out}
	case value.KindPath:
		nodes, rels, _ := v.AsPath()
		ns := make([]any, len(nodes))
		for i, n := range nodes {
			ns[i] = json.Number(strconv.FormatUint(uint64(n), 10))
		}
		rs := make([]any, len(rels))
		for i, r := range rels {
			rs[i] = json.Number(strconv.FormatUint(uint64(r), 10))
		}
		return map[string]any{"path": map[string]any{"nodes": ns, "rels": rs}}
	}
	return nil
}

// decodeValue is the inverse for the parameter values a record carries.
func decodeValue(t *testing.T, raw json.RawMessage) value.Value {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("bad param value %s: %v", raw, err)
	}
	switch x := v.(type) {
	case nil:
		return value.Null()
	case bool:
		return value.Bool(x)
	case string:
		return value.Str(x)
	case json.Number:
		i, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			t.Fatalf("non-integer bare number %s (floats are {\"float\": ...})", x)
		}
		return value.Int(i)
	case map[string]any:
		if f, ok := x["float"].(string); ok {
			g, err := strconv.ParseFloat(f, 64)
			if err != nil {
				t.Fatalf("bad float %q", f)
			}
			return value.Float(g)
		}
	}
	t.Fatalf("unsupported param encoding %s", raw)
	return value.Null()
}

// canonicalRow is the byte-canonical form used for comparison: the
// expected side re-marshals its decoded (UseNumber) tree, the produced
// side marshals its encoding; both sort object keys via encoding/json.
func canonicalRow(t *testing.T, row any) string {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return string(b)
}

// expectedRows decodes a record's rows into canonical strings.
func expectedRows(t *testing.T, rec *xcheckRecord) []string {
	t.Helper()
	out := make([]string, len(rec.Rows))
	for i, raw := range rec.Rows {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("bad row %s: %v", raw, err)
		}
		out[i] = canonicalRow(t, v)
	}
	return out
}

// producedRows runs the record's query and encodes the rows canonically.
func producedRows(t *testing.T, g *chickpeas.Snapshot, rec *xcheckRecord) []string {
	t.Helper()
	params := map[string]value.Value{}
	for k, raw := range rec.Params {
		params[k] = decodeValue(t, raw)
	}
	rows, err := RunWithParams(g, *rec.GQL, params)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", *rec.GQL, err)
	}
	if got := rows.Columns(); !slices.Equal(got, rec.Columns) {
		t.Fatalf("columns = %v, want %v (%s)", got, rec.Columns, *rec.GQL)
	}
	var out []string
	for r := range rows.All() {
		enc := make([]any, len(r.Values()))
		for i, v := range r.Values() {
			enc[i] = encodeValue(v)
		}
		out = append(out, canonicalRow(t, enc))
	}
	return out
}

// loadGraph resolves a record's fixture: an .rcpg next to the corpus file
// wins (the Rust-exported form); otherwise a built-in seed builder, round-
// tripped through the RCPG codec so the harness always consumes what the
// real corpus path would.
func loadGraph(t *testing.T, dir, name string) *chickpeas.Snapshot {
	t.Helper()
	if g, err := chickpeas.ReadRCPGFile(filepath.Join(dir, name+".rcpg")); err == nil {
		return g
	}
	build, ok := xcheckBuilders[name]
	if !ok {
		t.Fatalf("unknown fixture graph %q (no .rcpg beside the corpus, no seed builder)", name)
	}
	path := filepath.Join(t.TempDir(), name+".rcpg")
	if err := build(t).WriteRCPGFile(path); err != nil {
		t.Fatal(err)
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestXCheckCorpus(t *testing.T) {
	root := filepath.Join("testdata", "xcheck")
	if os.Getenv("GOCHICKPEAS_XCHECK_REGEN") != "" {
		regenSeedCorpus(t, filepath.Join(root, "seed"))
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		t.Skip("no corpus files under testdata/xcheck (Rust exporter not yet vendored, seed not generated)")
	}
	graphs := map[string]*chickpeas.Snapshot{}
	total := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var recs []xcheckRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		dir := filepath.Dir(path)
		for i := range recs {
			rec := &recs[i]
			if rec.GQL == nil {
				continue // untranslated Rust-exported record
			}
			total++
			key := dir + "|" + rec.Graph
			g, ok := graphs[key]
			if !ok {
				g = loadGraph(t, dir, rec.Graph)
				graphs[key] = g
			}
			want := expectedRows(t, rec)
			for _, interp := range []bool{false, true} {
				forceInterp = interp
				got := producedRows(t, g, rec)
				forceInterp = false
				if rec.Unordered {
					slices.Sort(got)
					want2 := slices.Clone(want)
					slices.Sort(want2)
					want = want2
				}
				if !slices.Equal(got, want) {
					t.Fatalf("%s record %d (interp=%v):\n  gql:  %s\n  got:  %v\n  want: %v",
						path, i, interp, *rec.GQL, got, want)
				}
			}
		}
	}
	t.Logf("xcheck: %d records verified across %d files", total, len(files))
}
