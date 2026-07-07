// weightedexport materializes the BI weighted-shortest-path relations
// (q15weight, interactsWith, cohort, ic14weight -- each Person->Person
// with a float `w` property, both directions per undirected knows pair)
// onto a canonical LDBC rcpg, writing a sibling weighted rcpg the GQL
// manifest can point BI Q15/Q19/Q20 (and later IC14) at. The derivations
// are the exact maps the native kernels traverse (internal/ldbc/
// weights.go), which are parity-gated against the rcp references.
//
//	go run ./cmd/weightedexport \
//	  -in  ~/rustychickpeas-ldbc/export/sf1_canonical.rcpg \
//	  -out ~/rustychickpeas-ldbc/export/sf1_weighted.rcpg
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

func main() {
	in := flag.String("in", "", "canonical LDBC rcpg to read")
	out := flag.String("out", "", "weighted rcpg to write")
	flag.Parse()
	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "weightedexport:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	t0 := time.Now()
	g, err := chickpeas.ReadRCPGFile(in)
	if err != nil {
		return fmt.Errorf("reading %s: %w", in, err)
	}
	fmt.Printf("loaded %s in %.1fs: %d nodes, %d rels\n",
		in, time.Since(t0).Seconds(), g.NodeCount(), g.RelCount())

	t := time.Now()
	rels, err := ldbc.DeriveWeightRels(g)
	if err != nil {
		return err
	}
	fmt.Printf("derived weights in %.1fs\n", time.Since(t).Seconds())

	t = time.Now()
	b := chickpeas.NewBuilderFromSnapshot(g)
	for _, typ := range []string{"q15weight", "interactsWith", "cohort", "ic14weight"} {
		for _, e := range rels[typ] {
			idx, err := b.AddRel(e.From, e.To, typ)
			if err != nil {
				return fmt.Errorf("adding %s rel: %w", typ, err)
			}
			if err := b.SetRelPropAt(idx, "w", e.W); err != nil {
				return fmt.Errorf("setting w on %s rel: %w", typ, err)
			}
		}
		fmt.Printf("  %-14s %8d rels\n", typ, len(rels[typ]))
	}
	wg := b.Finalize()
	fmt.Printf("finalized in %.1fs: %d nodes, %d rels\n",
		time.Since(t).Seconds(), wg.NodeCount(), wg.RelCount())

	t = time.Now()
	if err := wg.WriteRCPGFile(out); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Printf("wrote %s in %.1fs\n", out, time.Since(t).Seconds())
	return nil
}
