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
// Alongside each timing it emits the viz artifacts of task 027: a
// per-query allocation profile (profiles_gochickpeas.jsonl) and, per
// MATCHed kernel, its sliced Go source (code_gochickpeas.jsonl), both in
// the schemas the ldbc side's import_gochickpeas.sh folds (tasks/266).
//
//	go run ./cmd/ldbcnativebench -manifest bench-out/native_variants.tsv
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// outcome is one manifest row's verdict for the run summary.
type outcome struct {
	row    ldbc.ManifestRow
	status string // MATCH / DIFF / SKIP
	detail string
	rows   int
}

func run() error {
	manifest := flag.String("manifest", os.Getenv("GOCHICKPEAS_NATIVE_MANIFEST"),
		"path to native_variants.tsv (default $GOCHICKPEAS_NATIVE_MANIFEST)")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	codeOut := flag.String("code-out", "bench-out/code_gochickpeas.jsonl", "append-only kernel-source JSONL output")
	profilesOut := flag.String("profiles-out", "bench-out/profiles_gochickpeas.jsonl", "append-only alloc-profile JSONL output")
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
		rows = slices.DeleteFunc(rows, func(r ldbc.ManifestRow) bool { return !keep[r.Query] })
	}
	if len(rows) == 0 {
		return fmt.Errorf("no manifest rows selected")
	}
	stamp, err := ldbc.HeadStamp()
	if err != nil {
		return err
	}
	cellID := ldbc.CellIdentity{Engine: "gochickpeas (go)", Shape: "native kernel"}

	var f, pf, cf *os.File
	var enc, profEnc, codeEnc *json.Encoder
	if !*verifyOnly {
		if f, enc, err = ldbc.AppendJSONL(*out); err != nil {
			return err
		}
		defer f.Close()
		if pf, profEnc, err = ldbc.AppendJSONL(*profilesOut); err != nil {
			return err
		}
		defer pf.Close()
		if cf, codeEnc, err = ldbc.AppendJSONL(*codeOut); err != nil {
			return err
		}
		defer cf.Close()
	}

	// One load per distinct graph, in first-seen manifest order.
	graphs := map[string]*chickpeas.Snapshot{}
	var outcomes []outcome
	emitted := 0
	for _, row := range rows {
		id := row.Family + "/" + row.Query
		if !ldbc.HasNativeKernel(row.Family, row.Query) {
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

		// Prepare is untimed (mirrors what the rcp-native harness
		// builds outside its timer); the runnable is what parity
		// checks and the timed loop measure. PrepareNative hides whether
		// the kernel is boxed or migrated to value.Value.
		pk, _, err := ldbc.PrepareNative(row, g)
		if err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		if err := pk.Run(); err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		match, detail, err := pk.Verify(row)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if !match {
			outcomes = append(outcomes, outcome{row: row, status: "DIFF", detail: detail})
			fmt.Printf("%-16s DIFF  %s\n", id, detail)
			continue
		}
		rowCount := pk.RowCount()
		outcomes = append(outcomes, outcome{row: row, status: "MATCH", rows: rowCount})
		if *verifyOnly {
			fmt.Printf("%-16s MATCH (%d rows)\n", id, rowCount)
			continue
		}

		samples, err := ldbc.TimeSamples(*runs, pk.Run)
		if err != nil {
			return fmt.Errorf("%s (timed run): %w", id, err)
		}
		rec := ldbc.CellRecord(row, cellID, stamp, samples, rowCount, g)
		if err := enc.Encode(rec); err != nil {
			return err
		}
		emitted++
		fmt.Printf("%-16s MATCH %9.3f ms  (min %.3f, p75 %.3f, n=%d, %d rows)\n",
			id, rec.Ms, rec.MsMin, rec.MsP75, rec.MsN, rowCount)

		nAllocs, nBytes, err := ldbc.MeasureAllocs(pk.Run)
		if err != nil {
			return fmt.Errorf("%s (profiled run): %w", id, err)
		}
		if err := profEnc.Encode(ldbc.ProfileRecord{
			Family: row.Family, Query: row.Query, Engine: "gochickpeas (go)",
			Allocs: nAllocs, Bytes: nBytes, Rows: rowCount, Measure: ldbc.ProfileMeasure,
			EngineCommit: stamp.Commit, EngineDate: stamp.Date,
		}); err != nil {
			return err
		}
	}

	// Kernel source for every MATCHed (family, query), sliced from the
	// embedded sources so the code shown is the code that ran.
	codeEmitted := 0
	if !*verifyOnly {
		matched := map[string]bool{}
		for _, o := range outcomes {
			if o.status == "MATCH" {
				matched[o.row.Family+"/"+o.row.Query] = true
			}
		}
		sources, err := ldbc.NativeKernelSources()
		if err != nil {
			return err
		}
		for _, ks := range sources {
			if !matched[ks.Family+"/"+ks.Query] {
				continue
			}
			if err := codeEnc.Encode(ldbc.CodeRecord{
				Family: ks.Family, Query: ks.Query, Engine: "gochickpeas (go)", Lang: "go",
				Source: ks.Source, SrcRef: ks.SrcRef,
				EngineCommit: stamp.Commit, EngineDate: stamp.Date,
			}); err != nil {
				return err
			}
			codeEmitted++
		}
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
	fmt.Printf("\n%d/%d MATCH, %d DIFF, %d SKIP; %d timing+profile pairs, %d source records emitted at %s\n",
		match, len(outcomes), diff, skip, emitted, codeEmitted, stamp.Commit)
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
