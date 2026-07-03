// gabench runs the six LDBC Graphalytics algorithms (BFS, PR, WCC,
// CDLP, LCC, SSSP) over .v/.e datasets and emits warm timings in the
// rustychickpeas-ldbc suite's JSONL schema as engine "gochickpeas (go)",
// Family "GA" -- next to rcp-native (rust). Validation gates emission:
// any algorithm with a <name>-<ALGO> reference file present must PASS
// (exact for BFS/CDLP, relabel-invariant for WCC, 1e-6 tolerance for
// PR/LCC/SSSP) before its timing is written; algorithms without a
// reference emit unvalidated with an empty parity field, matching the
// rcp-native harness.
//
//	go run ./cmd/gabench -data ~/rustychickpeas-ldbc/data/graphalytics -datasets wiki-Talk
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// algo is one Graphalytics algorithm: run computes the node-indexed
// output, check validates it against a parsed reference.
type algo struct {
	name  string
	run   func() any
	check func(out any, ref map[uint32]string) error
}

// algos binds the six algorithms to a loaded dataset's parameters.
func algos(ds *ldbc.GADataset) []algo {
	g := ds.Graph
	d := ds.Params.Directed
	resolve := func(v *uint32) uint32 {
		if v != nil {
			if n, ok := ds.Node(*v); ok {
				return n
			}
		}
		return 0
	}
	bfsSrc := resolve(ds.Params.BFSSource)
	ssspSrc := resolve(ds.Params.SSSPSource)
	return []algo{
		{
			name: "BFS",
			run:  func() any { return ldbc.GABFS(g, bfsSrc, d) },
			check: func(out any, ref map[uint32]string) error {
				return ldbc.GACheckExactI64(ds, out.([]int64), ref)
			},
		},
		{
			name: "PR",
			run:  func() any { return ldbc.GAPageRank(g, d, ds.Params.PRDamping, ds.Params.PRIterations) },
			check: func(out any, ref map[uint32]string) error {
				return ldbc.GACheckEpsilon(ds, out.([]float64), ref, 1e-6)
			},
		},
		{
			name: "WCC",
			run:  func() any { return ldbc.GAWCC(g) },
			check: func(out any, ref map[uint32]string) error {
				return ldbc.GACheckRelabel(ds, out.([]uint32), ref)
			},
		},
		{
			name: "CDLP",
			run:  func() any { return ldbc.GACDLPSeeded(g, d, ds.Params.CDLPIterations, ds.VertexOfNode) },
			check: func(out any, ref map[uint32]string) error {
				labels := out.([]uint32)
				asI64 := make([]int64, len(labels))
				for i, l := range labels {
					asI64[i] = int64(l)
				}
				return ldbc.GACheckExactI64(ds, asI64, ref)
			},
		},
		{
			name: "LCC",
			run:  func() any { return ldbc.GALCC(g, d) },
			check: func(out any, ref map[uint32]string) error {
				return ldbc.GACheckEpsilon(ds, out.([]float64), ref, 1e-6)
			},
		},
		{
			name: "SSSP",
			run:  func() any { return ldbc.GASSSP(g, ssspSrc, d) },
			check: func(out any, ref map[uint32]string) error {
				return ldbc.GACheckEpsilon(ds, out.([]float64), ref, 1e-6)
			},
		},
	}
}

// reference loads <dir>/<name>-<ALGO> when present.
func reference(dir, name, algoName string) (map[uint32]string, bool) {
	text, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s-%s", name, algoName)))
	if err != nil {
		return nil, false
	}
	return ldbc.ParseGAReference(string(text)), true
}

func run() error {
	data := flag.String("data", os.Getenv("GOCHICKPEAS_GA_DATA"),
		"graphalytics dataset dir (default $GOCHICKPEAS_GA_DATA)")
	datasets := flag.String("datasets", "wiki-Talk", "comma-separated dataset names")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	runs := flag.Int("runs", 3, "timed runs per validated algorithm (median emitted)")
	verifyOnly := flag.Bool("verify-only", false, "validate only; no timings, no emission")
	flag.Parse()
	if *data == "" {
		return fmt.Errorf("no data dir: pass -data or set GOCHICKPEAS_GA_DATA")
	}
	stamp, err := ldbc.HeadStamp()
	if err != nil {
		return err
	}

	var f *os.File
	var enc *json.Encoder
	if !*verifyOnly {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		f, err = os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}

	emitted, failed := 0, 0
	for _, name := range strings.Split(*datasets, ",") {
		name = strings.TrimSpace(name)
		t0 := time.Now()
		ds, err := ldbc.LoadGADataset(*data, name)
		if err != nil {
			return fmt.Errorf("loading %s: %w", name, err)
		}
		g := ds.Graph
		fmt.Printf("loaded %s in %.1fs: %d nodes, %d rels, directed=%v\n",
			name, time.Since(t0).Seconds(), g.NodeCount(), g.RelCount(), ds.Params.Directed)

		for _, a := range algos(ds) {
			id := name + "/" + a.name
			// Validation run (doubles as warmup).
			t0 := time.Now()
			result := a.run()
			firstMS := float64(time.Since(t0).Microseconds()) / 1000.0
			parity := ""
			if ref, ok := reference(*data, name, a.name); ok {
				if err := a.check(result, ref); err != nil {
					failed++
					fmt.Printf("%-24s FAIL  %v\n", id, err)
					continue
				}
				parity = "MATCH"
				fmt.Printf("%-24s PASS  %9.3f ms\n", id, firstMS)
			} else {
				fmt.Printf("%-24s no reference; emitting unvalidated (%9.3f ms)\n", id, firstMS)
			}
			if *verifyOnly {
				continue
			}
			samples := make([]float64, *runs)
			for i := range samples {
				t0 := time.Now()
				a.run()
				samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
			}
			slices.Sort(samples)
			rec := ldbc.Record{
				Family:         "GA",
				Query:          a.name,
				Variant:        "committed",
				Engine:         "gochickpeas (go)",
				Warmth:         "warm",
				Ms:             ldbc.Percentile(samples, 0.5),
				Rows:           ds.Len(),
				SF:             1,
				Shape:          "native kernel",
				Parity:         parity,
				EngineCommit:   stamp.Commit,
				EngineDate:     stamp.Date,
				EngineDateTime: stamp.DateTime,
				EngineSubject:  stamp.Subject,
				MeasuredDate:   time.Now().UTC().Format("2006-01-02"),
				Source:         "emitted",
				MsMin:          samples[0],
				MsP25:          ldbc.Percentile(samples, 0.25),
				MsP75:          ldbc.Percentile(samples, 0.75),
				MsN:            len(samples),
				Meta: ldbc.Meta{
					Port:      "gochickpeas",
					Graph:     name,
					GoVersion: runtime.Version(),
					Nodes:     g.NodeCount(),
					Rels:      g.RelCount(),
				},
			}
			if err := enc.Encode(rec); err != nil {
				return err
			}
			emitted++
			fmt.Printf("%-24s %10.3f ms  (min %.3f, p75 %.3f, n=%d)\n",
				id, rec.Ms, rec.MsMin, rec.MsP75, rec.MsN)
		}
	}
	fmt.Printf("\n%d records emitted at %s\n", emitted, stamp.Commit)
	if failed > 0 {
		return fmt.Errorf("%d algorithms failed validation", failed)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gabench:", err)
		os.Exit(1)
	}
}
