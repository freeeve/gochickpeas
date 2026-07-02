// gencorpus writes the RCPG conformance corpus using this module's own
// builders and writer, for the reverse-direction interop test: the Rust
// codec (rustychickpeas-format's go_interop test, gated on
// RCPG_INTEROP_CORPUS) parses these files and requires bit-exact equality
// with the graphs its corpus defines.
//
// Usage: gencorpus [dir]   (default: conformance-out)
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeeve/gochickpeas/rcpg/internal/conformance"
)

func main() {
	dir := "conformance-out"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := run(dir); err != nil {
		fmt.Fprintln(os.Stderr, "gencorpus:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, c := range conformance.Corpus() {
		b, err := conformance.EncodeCase(c)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		path := filepath.Join(dir, c.Name+".rcpg")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s (%d bytes)\n", path, len(b))
	}
	return nil
}
