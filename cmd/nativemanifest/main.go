// nativemanifest generates the interim native parity manifest
// (native_variants.tsv) for cmd/ldbcnativebench by hashing the
// rustychickpeas-ldbc committed reference rows (python/refs) with
// rowhash/v1 -- the same refs and hash the GQL manifest carries, so a
// native kernel and its GQL twin gate against the identical oracle.
// The authoritative manifest will eventually be authored ldbc-side
// (their tasks/263, viz/native_manifest.py); this tool exists so the
// Go kernels are verifiable before that lands, and its output follows
// the agreed 6-column shape exactly:
//
//	family <TAB> query <TAB> variant <TAB> graph <TAB> refhash <TAB> norm
//
//	go run ./cmd/nativemanifest -ldbc ~/rustychickpeas-ldbc
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// entry pins one manifest row: which ref file and norm ops gate the
// query, and which graph key it runs on. Norms for GQL-twin rows match
// gql_variants.tsv so both manifests stay in lockstep; native-only rows
// (BI Q4/Q8/Q9/Q10/Q15/Q17/Q19/Q20, IC14, FinBench CR8, SPB) carry the
// norm the kernel's output shape needs against the stored ref. SPB rows
// set key: their 30 oracles share one combined parity JSON, and the
// refhash comes from that query's block (order-free full result sets --
// rowhash's sorted multiset needs no norm).
type entry struct {
	family string
	query  string
	ref    string
	key    string
	norm   string
	graph  string
}

// graph keys resolved against -ldbc/export; the SPB graph is this
// repo's own spbexport output (their tasks/263 export hasn't shipped),
// resolved against -spb-graph instead.
const (
	sf1      = "sf1_canonical.rcpg"
	finbench = "finbench_sf10_canonical.rcpg"
	spbGraph = "spb_canonical.rcpg"
)

// spbQueries lists the SPB family in their parity JSON's canonical
// order: basics q1-q5/q7/q9, advanced a1-a10/a13-a25 (q6/q8 exist only
// in the sample-vocabulary demo; a17/a20 are their real-data versions).
// Ids stay lowercase to join the ldbc viz's SPB rows.
func spbQueries() []string {
	qs := []string{"q1", "q2", "q3", "q4", "q5", "q7", "q9"}
	for i := 1; i <= 25; i++ {
		if i == 11 || i == 12 {
			continue
		}
		qs = append(qs, fmt.Sprintf("a%d", i))
	}
	return qs
}

// entries lists the native manifest coverage in family/query order. BI
// Q16 gates on the official (empty at SF1) A-and-B intersection ref,
// like the GQL manifest.
func entries() []entry {
	var out []entry
	add := func(family, query, ref, norm, graph string) {
		out = append(out, entry{family: family, query: query, ref: ref, norm: norm, graph: graph})
	}
	// BI Q1-Q20; Q3 col2 is forum.creationDate emitted in epoch-ms
	// against a ref stored in epoch-days (same as the GQL twin).
	for _, e := range [][2]string{
		{"Q1", "-"}, {"Q2", "-"}, {"Q3", "col2:msday"}, {"Q4", "-"},
		{"Q5", "-"}, {"Q6", "-"}, {"Q7", "-"}, {"Q8", "-"},
		{"Q9", "-"}, {"Q10", "-"}, {"Q11", "-"}, {"Q12", "-"},
		{"Q13", "-"}, {"Q14", "-"}, {"Q15", "-"}, {"Q16", "-"},
		{"Q17", "-"}, {"Q18", "-"}, {"Q19", "-"}, {"Q20", "-"},
	} {
		add("BI", e[0], fmt.Sprintf("bi/%s.rust.json", lower(e[0])), e[1], sf1)
	}
	// IC1-14 + IS1-7.
	for i := 1; i <= 14; i++ {
		add("IC", fmt.Sprintf("IC%d", i), fmt.Sprintf("ic/ic%d.rust.json", i), "-", sf1)
	}
	for i := 1; i <= 7; i++ {
		add("IC", fmt.Sprintf("IS%d", i), fmt.Sprintf("ic/is%d.rust.json", i), "-", sf1)
	}
	// FinBench CR1-12 + SR1-6 (SF10).
	for i := 1; i <= 12; i++ {
		norm := "round3"
		switch i {
		case 5:
			norm = "unwrap1"
		case 8:
			norm = "round3"
		}
		add("FinBench", fmt.Sprintf("CR%d", i), fmt.Sprintf("finbench/cr%d.rust.json", i), norm, finbench)
	}
	for i := 1; i <= 6; i++ {
		add("FinBench", fmt.Sprintf("SR%d", i), fmt.Sprintf("finbench/sr%d.rust.json", i), "round3", finbench)
	}
	// SPB q1-a25 (30): one combined ref, per-query key, no norm.
	for _, q := range spbQueries() {
		out = append(out, entry{
			family: "SPB", query: q, ref: "spb/spb.parity.rust.json",
			key: q, norm: "-", graph: spbGraph,
		})
	}
	return out
}

// lower maps a BI query id (Q1) to its ref basename (q1).
func lower(q string) string {
	return "q" + q[1:]
}

func run() error {
	root := flag.String("ldbc", os.Getenv("GOCHICKPEAS_LDBC_ROOT"),
		"rustychickpeas-ldbc checkout root (default $GOCHICKPEAS_LDBC_ROOT)")
	out := flag.String("out", "bench-out/native_variants.tsv", "manifest output path")
	spbDir := flag.String("spb-graph-dir", "export", "directory holding this repo's spbexport output")
	flag.Parse()
	if *root == "" {
		return fmt.Errorf("no ldbc root: pass -ldbc or set GOCHICKPEAS_LDBC_ROOT")
	}
	refs := filepath.Join(*root, "python", "refs")
	export := filepath.Join(*root, "export")

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "# native_variants (rowhash/v1); cols: family query variant graph refhash norm; interim -- authoritative manifest lands ldbc-side (their tasks/263)")
	n := 0
	byFamily := map[string]int{}
	for _, e := range entries() {
		var hash string
		if e.key != "" {
			hash, err = ldbc.SPBRefHash(filepath.Join(refs, e.ref), e.key)
		} else {
			hash, err = ldbc.RefHash(filepath.Join(refs, e.ref))
		}
		if err != nil {
			return fmt.Errorf("%s/%s: %w", e.family, e.query, err)
		}
		graphDir := export
		if e.family == "SPB" {
			graphDir = *spbDir
		}
		graph, err := filepath.Abs(filepath.Join(graphDir, e.graph))
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "%s\t%s\tcanonical\t%s\t%s\t%s\n", e.family, e.query, graph, hash, e.norm)
		n++
		byFamily[e.family]++
	}
	fmt.Printf("wrote %d native variants -> %s  (%v)\n", n, *out, byFamily)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nativemanifest:", err)
		os.Exit(1)
	}
}
