// loadbench times graph loads and appends family=LOAD records to the
// suite's JSONL, mirroring the rcp-native (rust) load emissions: query
// is the workload key (BI/FINBENCH/SPB), variant the on-disk format
// (rcpg, or nt for the SPB RDF path), rows = nodes + rels, and meta
// carries format/bytes/mb_s/rec_s. Median of -runs (first load also
// warms the page cache, matching the rust side's warm loads).
//
//	go run ./cmd/loadbench -ldbc ~/rustychickpeas-ldbc
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	cases := []loadCase{
		{"BI", "rcpg", filepath.Join(*root, "export", "sf1_canonical.rcpg"), 1, rcpg},
		{"FINBENCH", "rcpg", filepath.Join(*root, "export", "finbench_sf10_canonical.rcpg"), 10, rcpg},
		{"SPB", "rcpg", filepath.Join(*spbDir, "spb_canonical.rcpg"), 1, rcpg},
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
		rows := int(g.NodeCount()) + int(g.RelCount())
		rec := ldbc.Record{
			Family:       "LOAD",
			Query:        c.query,
			Variant:      c.variant,
			Engine:       "gochickpeas (go)",
			Warmth:       "warm",
			Ms:           ms,
			Rows:         rows,
			SF:           c.sf,
			Shape:        "load",
			EngineCommit: stamp.Commit, EngineDate: stamp.Date,
			EngineDateTime: stamp.DateTime, EngineSubject: stamp.Subject,
			MeasuredDate: time.Now().UTC().Format("2006-01-02"),
			Source:       "emitted",
			MsMin:        samples[0],
			MsP25:        ldbc.Percentile(samples, 0.25),
			MsP75:        ldbc.Percentile(samples, 0.75),
			MsN:          len(samples),
			Meta: ldbc.Meta{
				Port: "gochickpeas", Graph: filepath.Base(c.path),
				GoVersion: runtime.Version(),
				Nodes:     g.NodeCount(), Rels: g.RelCount(),
				Format: c.variant, Bytes: info.Size(),
				MbS:  float64(info.Size()) / (1 << 20) / (ms / 1000.0),
				RecS: int64(float64(rows) / (ms / 1000.0)),
			},
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		fmt.Printf("LOAD/%-9s %-5s %9.1f ms  (%d nodes, %d rels, %.1f MB/s, n=%d)\n",
			c.query, c.variant, ms, g.NodeCount(), g.RelCount(), rec.Meta.MbS, len(samples))
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadbench:", err)
		os.Exit(1)
	}
}
