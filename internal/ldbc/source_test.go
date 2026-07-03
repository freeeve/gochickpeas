package ldbc

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestNativeKernelSources(t *testing.T) {
	srcs, err := NativeKernelSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != NativeKernelCount() {
		t.Fatalf("got %d sources, registry has %d", len(srcs), NativeKernelCount())
	}
	seen := map[string]bool{}
	for _, ks := range srcs {
		id := ks.Family + "/" + ks.Query
		if seen[id] {
			t.Errorf("%s: duplicate source record", id)
		}
		seen[id] = true
		checkSlice(t, ks)
	}
}

func TestGAKernelSources(t *testing.T) {
	srcs, err := GAKernelSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != len(gaKernelFuncs) {
		t.Fatalf("got %d GA sources, want %d", len(srcs), len(gaKernelFuncs))
	}
	for _, ks := range srcs {
		if ks.Family != "GA" {
			t.Errorf("%s: family %q, want GA", ks.Query, ks.Family)
		}
		checkSlice(t, ks)
	}
}

// checkSlice verifies the slice text and its file:line ref against the
// on-disk source: the embed and the working tree must agree so srcRef
// stays accurate against the swept commit.
func checkSlice(t *testing.T, ks KernelSource) {
	t.Helper()
	funcLine := "func " + ks.Func + "("
	if !strings.Contains(ks.Source, funcLine) {
		t.Errorf("%s/%s: source does not contain %q", ks.Family, ks.Query, funcLine)
	}
	if !strings.HasSuffix(strings.TrimRight(ks.Source, "\n"), "}") {
		t.Errorf("%s/%s: source does not end at a closing brace", ks.Family, ks.Query)
	}
	rest, ok := strings.CutPrefix(ks.SrcRef, "internal/ldbc/")
	if !ok {
		t.Fatalf("%s/%s: srcRef %q not repo-relative", ks.Family, ks.Query, ks.SrcRef)
	}
	file, lineStr, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatalf("%s/%s: srcRef %q has no line", ks.Family, ks.Query, ks.SrcRef)
	}
	line, err := strconv.Atoi(lineStr)
	if err != nil {
		t.Fatalf("%s/%s: srcRef line %q: %v", ks.Family, ks.Query, lineStr, err)
	}
	disk, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(disk), "\n")
	if line < 1 || line > len(lines) {
		t.Fatalf("%s: line %d out of range", ks.SrcRef, line)
	}
	if !strings.HasPrefix(lines[line-1], funcLine) {
		t.Errorf("%s: line is %q, want prefix %q", ks.SrcRef, lines[line-1], funcLine)
	}
}

var allocSink []byte

func TestMeasureAllocs(t *testing.T) {
	nAllocs, nBytes, err := MeasureAllocs(func() error {
		allocSink = make([]byte, 1<<20)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if nAllocs < 1 {
		t.Errorf("allocs = %d, want >= 1", nAllocs)
	}
	if nBytes < 1<<20 {
		t.Errorf("bytes = %d, want >= %d", nBytes, 1<<20)
	}
}

func TestMeasureAllocsError(t *testing.T) {
	boom := errors.New("boom")
	if _, _, err := MeasureAllocs(func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
