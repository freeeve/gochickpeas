// Per-kernel benchmark over a real exported graph, env-gated like the GA
// benches: set GOCHICKPEAS_SF1_RCPG to a .rcpg path and (optionally)
// GOCHICKPEAS_KERNEL to "Family/Query" (default IC/IC5, task 068's
// biggest-absolute cell). Drives profiling of the shared kernel
// primitives: go test ./internal/ldbc -bench NativeKernel -cpuprofile ...
package ldbc

import (
	"os"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func BenchmarkNativeKernel(b *testing.B) {
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		b.Skip("GOCHICKPEAS_SF1_RCPG not set")
	}
	id := os.Getenv("GOCHICKPEAS_KERNEL")
	if id == "" {
		id = "IC/IC5"
	}
	fam, query, ok := strings.Cut(id, "/")
	if !ok {
		b.Fatalf("GOCHICKPEAS_KERNEL %q: want Family/Query", id)
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		b.Fatal(err)
	}
	pk, ok, err := PrepareNative(ManifestRow{Family: fam, Query: query}, g)
	if !ok {
		b.Fatalf("no kernel %s", id)
	}
	if err != nil {
		b.Fatal(err)
	}
	if err := pk.Run(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pk.Run(); err != nil {
			b.Fatal(err)
		}
	}
}
