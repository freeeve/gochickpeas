// ldbcnativebench runs the native parity manifest (native_variants.tsv,
// rustychickpeas-ldbc task 263 / gochickpeas task 025) against the
// per-query native Go kernels: each row's kernel runs on the row's
// graph, result rows are normalized and hashed per rowhash/v1, and only
// a hash-equal (MATCH) run may emit a timing -- as engine
// "gochickpeas (go)" in the suite's JSONL schema, joining the
// BI/IC/FinBench families next to rcp-native (rust). Rows without a
// registered kernel skip loudly (coverage is incremental by design); a
// DIFF fails the run.
//
//	go run ./cmd/ldbcnativebench -manifest bench-out/native_variants.tsv
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

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// outcome is one manifest row's verdict for the run summary.
type outcome struct {
	row    ldbc.NativeRow
	status string // MATCH / DIFF / SKIP
	detail string
	rows   int
}

func run() error {
	manifest := flag.String("manifest", os.Getenv("GOCHICKPEAS_NATIVE_MANIFEST"),
		"path to native_variants.tsv (default $GOCHICKPEAS_NATIVE_MANIFEST)")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	runs := flag.Int("runs", 5, "timed runs per matched query (median emitted)")
	only := flag.String("only", "", "comma-separated query ids (e.g. Q1,IC3); empty = all")
	verifyOnly := flag.Bool("verify-only", false, "check parity only; no timings, no emission")
	flag.Parse()
	if *manifest == "" {
		return fmt.Errorf("no manifest: pass -manifest or set GOCHICKPEAS_NATIVE_MANIFEST")
	}

	rows, err := ldbc.LoadNativeManifest(*manifest)
	if err != nil {
		return err
	}
	if *only != "" {
		keep := map[string]bool{}
		for id := range strings.SplitSeq(*only, ",") {
			keep[strings.TrimSpace(id)] = true
		}
		rows = slices.DeleteFunc(rows, func(r ldbc.NativeRow) bool { return !keep[r.Query] })
	}
	if len(rows) == 0 {
		return fmt.Errorf("no manifest rows selected")
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

	// One load per distinct graph, in first-seen manifest order.
	graphs := map[string]*chickpeas.Snapshot{}
	var outcomes []outcome
	emitted := 0
	for _, row := range rows {
		id := row.Family + "/" + row.Query
		kernel, ok := ldbc.NativeKernelFor(row.Family, row.Query)
		if !ok {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: "no native kernel yet"})
			fmt.Printf("%-16s SKIP  no native kernel yet\n", id)
			continue
		}
		g, ok := graphs[row.Graph]
		if !ok {
			t0 := time.Now()
			g, err = chickpeas.ReadRCPGFile(row.Graph)
			if err != nil {
				return fmt.Errorf("loading %s: %w", row.Graph, err)
			}
			fmt.Printf("loaded %s in %.1fs: %d nodes, %d rels\n",
				filepath.Base(row.Graph), time.Since(t0).Seconds(), g.NodeCount(), g.RelCount())
			graphs[row.Graph] = g
		}

		cells, err := kernel(g)
		if err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		normed, err := ldbc.ApplyNorm(cells, row.Norm)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		hash, err := ldbc.RowsHash(normed)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if hash != row.RefHash {
			detail := fmt.Sprintf("hash %s != ref %s (%d rows)", hash, row.RefHash, len(cells))
			outcomes = append(outcomes, outcome{row: row, status: "DIFF", detail: detail})
			fmt.Printf("%-16s DIFF  %s\n", id, detail)
			continue
		}
		outcomes = append(outcomes, outcome{row: row, status: "MATCH", rows: len(cells)})
		if *verifyOnly {
			fmt.Printf("%-16s MATCH (%d rows)\n", id, len(cells))
			continue
		}

		samples := make([]float64, *runs)
		for i := range samples {
			t0 := time.Now()
			if _, err := kernel(g); err != nil {
				return fmt.Errorf("%s (timed run): %w", id, err)
			}
			samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
		}
		// The suite runs FinBench at SF10, everything else here at SF1
		// (the manifest's graph column implies it; the record schema
		// wants it explicit).
		sf := 1
		if row.Family == "FinBench" {
			sf = 10
		}
		slices.Sort(samples)
		rec := ldbc.Record{
			Family:         row.Family,
			Query:          row.Query,
			Variant:        row.Variant,
			Engine:         "gochickpeas (go)",
			Warmth:         "warm",
			Ms:             ldbc.Percentile(samples, 0.5),
			Rows:           len(cells),
			SF:             sf,
			Shape:          "native kernel",
			Parity:         "MATCH",
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
				Graph:     filepath.Base(row.Graph),
				GoVersion: runtime.Version(),
				Nodes:     g.NodeCount(),
				Rels:      g.RelCount(),
			},
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		emitted++
		fmt.Printf("%-16s MATCH %9.3f ms  (min %.3f, p75 %.3f, n=%d, %d rows)\n",
			id, rec.Ms, rec.MsMin, rec.MsP75, rec.MsN, len(cells))
	}

	match, diff, skip := 0, 0, 0
	for _, o := range outcomes {
		switch o.status {
		case "MATCH":
			match++
		case "DIFF":
			diff++
		case "SKIP":
			skip++
		}
	}
	fmt.Printf("\n%d/%d MATCH, %d DIFF, %d SKIP; %d records emitted at %s\n",
		match, len(outcomes), diff, skip, emitted, stamp.Commit)
	if diff > 0 {
		return fmt.Errorf("%d queries DIFFed against their reference hashes", diff)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ldbcnativebench:", err)
		os.Exit(1)
	}
}
