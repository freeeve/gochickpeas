// spbexport builds the SPB property graph from the LDBC SPB N-Triples
// extract and writes it as a canonical .rcpg for the native kernels
// (task 026). The RDF -> property-graph mapping mirrors
// rustychickpeas-ldbc src/spb/loader.rs exactly (see internal/ldbc
// spbload.go); the printed banner carries the same counts as the Rust
// loader's, which is the load-level parity check until their tasks/263
// ships an authoritative export.
//
//	go run ./cmd/spbexport -ldbc ~/rustychickpeas-ldbc -out export/spb_canonical.rcpg
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/freeeve/gochickpeas/internal/ldbc"
)

func run() error {
	root := flag.String("ldbc", os.Getenv("GOCHICKPEAS_LDBC_ROOT"),
		"rustychickpeas-ldbc checkout root (default $GOCHICKPEAS_LDBC_ROOT)")
	in := flag.String("in", "", "N-Triples/N-Quads input (default <ldbc>/data/spb/extract/spb-validate.nt)")
	out := flag.String("out", "export/spb_canonical.rcpg", "output .rcpg path")
	flag.Parse()

	src := *in
	if src == "" {
		if *root == "" {
			return fmt.Errorf("no input: pass -in, or -ldbc / $GOCHICKPEAS_LDBC_ROOT for the default extract")
		}
		src = filepath.Join(*root, "data", "spb", "extract", "spb-validate.nt")
	}

	fmt.Printf("loading %s ...\n", src)
	t := time.Now()
	g, stats, err := ldbc.LoadSPBFile(src)
	if err != nil {
		return err
	}
	fmt.Printf("loaded %d resources from %d triples (%d rels, %d literal props) in %.2fs\n",
		stats.Resources, stats.Triples, stats.Rels, stats.Literals, time.Since(t).Seconds())
	for _, label := range []string{"CreativeWork", "Feature"} {
		n := 0
		if set, ok := g.NodesWithLabel(label); ok {
			n = set.Len()
		}
		fmt.Printf("  %s: %d\n", label, n)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	t = time.Now()
	if err := g.WriteRCPGFile(*out); err != nil {
		return err
	}
	info, err := os.Stat(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%.1f MB) in %.2fs\n", *out, float64(info.Size())/(1<<20), time.Since(t).Seconds())
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spbexport:", err)
		os.Exit(1)
	}
}
