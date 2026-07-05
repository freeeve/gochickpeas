// loadbench times graph loads and appends family=LOAD records to the
// suite's JSONL, mirroring the rcp-native (rust) load emissions: query
// is the workload key (BI/FINBENCH/SPB), variant the on-disk format
// (rcpg, or nt for the SPB RDF path), rows = nodes + rels, and meta
// carries format/bytes/mb_s/rec_s. Median of -runs (first load also
// warms the page cache, matching the rust side's warm loads).
//
// Every record carries a parity verdict (task 029): rcpg IS the
// canonical snapshot so those cases stamp MATCH, while the SPB nt reload
// is structurally diffed (ldbc.DiffGraphs) against the rcpg baseline --
// spbexport built that baseline from the same spb-validate.nt, so a
// correct RDF loader reports MATCH and a divergent one is drawn WRONG on
// the ldbc Loads heatmap, never a silent throughput win.
//
//	go run ./cmd/loadbench -ldbc ~/rustychickpeas-ldbc
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// loadCase is one LOAD cell: a workload graph in one on-disk format.
type loadCase struct {
	query   string // workload key, matching the rust LOAD rows
	variant string // format: rcpg | nt
	path    string
	sf      int
	load    func(path string) (*chickpeas.Snapshot, error)
}

func run() error {
	root := flag.String("ldbc", os.Getenv("GOCHICKPEAS_LDBC_ROOT"),
		"rustychickpeas-ldbc checkout root (default $GOCHICKPEAS_LDBC_ROOT)")
	spbDir := flag.String("spb-graph-dir", "export", "directory holding this repo's spbexport output")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	runs := flag.Int("runs", 3, "loads per case (median emitted)")
	flag.Parse()
	if *root == "" {
		return fmt.Errorf("no ldbc root: pass -ldbc or set $GOCHICKPEAS_LDBC_ROOT")
	}

	rcpg := func(path string) (*chickpeas.Snapshot, error) { return chickpeas.ReadRCPGFile(path) }
	nt := func(path string) (*chickpeas.Snapshot, error) {
		g, _, err := ldbc.LoadSPBFile(path)
		return g, err
	}
	spbRcpg := filepath.Join(*spbDir, "spb_canonical.rcpg")
	cases := []loadCase{
		{"BI", "rcpg", filepath.Join(*root, "export", "sf1_canonical.rcpg"), 1, rcpg},
		{"FINBENCH", "rcpg", filepath.Join(*root, "export", "finbench_sf10_canonical.rcpg"), 10, rcpg},
		{"SPB", "rcpg", spbRcpg, 1, rcpg},
		{"SPB", "nt", filepath.Join(*root, "data", "spb", "extract", "spb-validate.nt"), 1, nt},
	}

	stamp, err := ldbc.HeadStamp()
	if err != nil {
		return err
	}
	f, enc, err := ldbc.AppendJSONL(*out)
	if err != nil {
		return err
	}
	defer f.Close()

	var spbRef *chickpeas.Snapshot
	for _, c := range cases {
		info, err := os.Stat(c.path)
		if err != nil {
			fmt.Printf("LOAD/%-9s %-5s SKIP  %v\n", c.query, c.variant, err)
			continue
		}
		samples := make([]float64, *runs)
		var g *chickpeas.Snapshot
		for i := range samples {
			t0 := time.Now()
			if g, err = c.load(c.path); err != nil {
				return fmt.Errorf("LOAD/%s %s: %w", c.query, c.variant, err)
			}
			samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
		}
		slices.Sort(samples)
		ms := ldbc.Percentile(samples, 0.5)
		parity := "MATCH" // rcpg IS the canonical snapshot (the diff baseline)
		if c.query == "SPB" && c.variant == "rcpg" {
			spbRef = g
		}
		if c.variant == "nt" {
			if parity, err = spbNTParity(spbRef, spbRcpg, g); err != nil {
				fmt.Printf("LOAD/%-9s %-5s WARN  no diff baseline: %v\n", c.query, c.variant, err)
			}
		}
		rows := int(g.NodeCount()) + int(g.RelCount())
		rec := ldbc.NewRecord(ldbc.RecordSpec{
			Family: "LOAD", Query: c.query, Variant: c.variant,
			Engine: "gochickpeas (go)", Shape: "load",
			SF: c.sf, Parity: parity, Rows: rows,
			Meta: ldbc.Meta{
				Graph: filepath.Base(c.path),
				Nodes: g.NodeCount(), Rels: g.RelCount(),
				Format: c.variant, Bytes: info.Size(),
				MbS:  float64(info.Size()) / (1 << 20) / (ms / 1000.0),
				RecS: int64(float64(rows) / (ms / 1000.0)),
			},
		}, stamp, samples)
		if err := enc.Encode(rec); err != nil {
			return err
		}
		fmt.Printf("LOAD/%-9s %-5s %9.1f ms  (%d nodes, %d rels, %.1f MB/s, n=%d)  %s\n",
			c.query, c.variant, ms, g.NodeCount(), g.RelCount(), rec.Meta.MbS, len(samples), parity)
	}
	return nil
}

// spbNTParity diff-gates the nt-reloaded SPB graph against the rcpg
// baseline -- reusing the SPB rcpg case's snapshot, or loading it when
// that case was skipped. The external id is the `uri` property every
// IRI node carries (the cross-engine key, cf. internal/ldbc/spbload.go);
// Full covers the whole id space since the SPB extract is small.
func spbNTParity(ref *chickpeas.Snapshot, rcpgPath string, g *chickpeas.Snapshot) (string, error) {
	if ref == nil {
		var err error
		if ref, err = chickpeas.ReadRCPGFile(rcpgPath); err != nil {
			return "", err
		}
	}
	if ok, detail := ldbc.DiffGraphs(ref, g, ldbc.DiffOpts{IDProp: "uri", Full: true}); !ok {
		return "DIFF (" + detail + ")", nil
	}
	return "MATCH", nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadbench:", err)
		os.Exit(1)
	}
}
