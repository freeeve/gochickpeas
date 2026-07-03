// Graphalytics dataset loading: <name>.v / <name>.e / <name>.properties
// into a Snapshot plus the dense-node <-> original-vertex maps and the
// per-algorithm parameters (LDBC Graphalytics spec v1.0.x, port of
// rustychickpeas-ldbc src/graphalytics/load.rs). Vertices get dense
// node ids in .v order; each .e line `src dst [weight]` becomes an `e`
// rel (the weight property is stored only when the column is present;
// readers default absent weights to 1.0, matching the Rust loader's
// unconditional 1.0 default).

package ldbc

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	chickpeas "github.com/freeeve/gochickpeas"
)

// GAParams are the per-dataset algorithm parameters from .properties.
// Sources are original vertex ids (resolve via GADataset.Node).
type GAParams struct {
	Directed       bool
	BFSSource      *uint32
	SSSPSource     *uint32
	PRDamping      float64
	PRIterations   int
	CDLPIterations int
}

// defaultGAParams mirrors the Rust Params::default.
func defaultGAParams() GAParams {
	return GAParams{Directed: true, PRDamping: 0.85, PRIterations: 10, CDLPIterations: 10}
}

// GADataset is a loaded dataset: snapshot, parameters, and the maps
// between dense node ids (0..n) and original vertex ids.
type GADataset struct {
	Graph        *chickpeas.Snapshot
	Params       GAParams
	VertexOfNode []uint32
	NodeOfVertex map[uint32]uint32
}

// Node is the dense node id for an original vertex id.
func (d *GADataset) Node(vertex uint32) (uint32, bool) {
	n, ok := d.NodeOfVertex[vertex]
	return n, ok
}

// Len is the vertex count.
func (d *GADataset) Len() int { return len(d.VertexOfNode) }

// LoadGADataset reads <dir>/<name>.{v,e,properties}; a missing
// .properties falls back to the defaults.
func LoadGADataset(dir, name string) (*GADataset, error) {
	v, err := os.ReadFile(filepath.Join(dir, name+".v"))
	if err != nil {
		return nil, err
	}
	e, err := os.ReadFile(filepath.Join(dir, name+".e"))
	if err != nil {
		return nil, err
	}
	props, _ := os.ReadFile(filepath.Join(dir, name+".properties"))
	return loadGAStr(string(v), string(e), string(props))
}

// loadGAStr builds a GADataset from in-memory file contents (the unit
// test seam for LoadGADataset).
func loadGAStr(vText, eText, props string) (*GADataset, error) {
	params := parseGAParams(props)

	var vertexOfNode []uint32
	nodeOfVertex := map[uint32]uint32{}
	vsc := bufio.NewScanner(strings.NewReader(vText))
	vsc.Buffer(make([]byte, 1<<20), 1<<20)
	for vsc.Scan() {
		tok := firstField(vsc.Text())
		if tok == "" {
			continue
		}
		vid64, err := strconv.ParseUint(tok, 10, 32)
		if err != nil {
			continue
		}
		vid := uint32(vid64)
		if _, dup := nodeOfVertex[vid]; !dup {
			nodeOfVertex[vid] = uint32(len(vertexOfNode))
			vertexOfNode = append(vertexOfNode, vid)
		}
	}

	n := len(vertexOfNode)
	b := chickpeas.NewBuilder(n, strings.Count(eText, "\n")+1)
	for range n {
		if _, err := b.AddNode("V"); err != nil {
			return nil, err
		}
	}
	esc := bufio.NewScanner(strings.NewReader(eText))
	esc.Buffer(make([]byte, 1<<20), 1<<20)
	for esc.Scan() {
		f0, f1, f2 := threeFields(esc.Text())
		if f0 == "" || f1 == "" {
			continue
		}
		sv, err1 := strconv.ParseUint(f0, 10, 32)
		dv, err2 := strconv.ParseUint(f1, 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		su, ok1 := nodeOfVertex[uint32(sv)]
		du, ok2 := nodeOfVertex[uint32(dv)]
		if !ok1 || !ok2 {
			continue
		}
		idx, err := b.AddRel(su, du, "e")
		if err != nil {
			return nil, err
		}
		if f2 != "" {
			if w, err := strconv.ParseFloat(f2, 64); err == nil {
				if err := b.SetRelPropAt(idx, "weight", w); err != nil {
					return nil, err
				}
			}
		}
	}
	return &GADataset{
		Graph:        b.Finalize(),
		Params:       params,
		VertexOfNode: vertexOfNode,
		NodeOfVertex: nodeOfVertex,
	}, nil
}

// firstField is the first whitespace-separated token of a line.
func firstField(s string) string {
	f0, _, _ := threeFields(s)
	return f0
}

// threeFields splits a line into its first three whitespace-separated
// tokens without allocating a slice per line (the .e file has millions
// of lines).
func threeFields(s string) (a, b, c string) {
	fields := [3]string{}
	i, n := 0, 0
	for i < len(s) && n < 3 {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
			i++
		}
		start := i
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\r' && s[i] != '\n' {
			i++
		}
		if i > start {
			fields[n] = s[start:i]
			n++
		}
	}
	return fields[0], fields[1], fields[2]
}

// parseGAParams reads the subset of .properties keys the algorithms
// need, matching on key suffix (the dataset-name prefix is irrelevant).
func parseGAParams(props string) GAParams {
	p := defaultGAParams()
	for line := range strings.Lines(props) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch {
		case strings.HasSuffix(key, ".directed"):
			p.Directed = strings.EqualFold(val, "true")
		case strings.HasSuffix(key, ".bfs.source-vertex"):
			if v, err := strconv.ParseUint(val, 10, 32); err == nil {
				u := uint32(v)
				p.BFSSource = &u
			}
		case strings.HasSuffix(key, ".sssp.source-vertex"):
			if v, err := strconv.ParseUint(val, 10, 32); err == nil {
				u := uint32(v)
				p.SSSPSource = &u
			}
		case strings.HasSuffix(key, ".pr.damping-factor"):
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				p.PRDamping = v
			}
		case strings.HasSuffix(key, ".pr.num-iterations"):
			if v, err := strconv.Atoi(val); err == nil {
				p.PRIterations = v
			}
		case strings.HasSuffix(key, ".cdlp.max-iterations"):
			if v, err := strconv.Atoi(val); err == nil {
				p.CDLPIterations = v
			}
		}
	}
	return p
}
