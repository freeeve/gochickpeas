// rssbench measures the resident memory footprint of a loaded graph
// (task 213). It loads a .rcpg into a Snapshot, settles the Go heap
// (GC + FreeOSMemory), and reports the process RSS, the Go heap
// attribution (runtime.MemStats), and the overhead ratio RSS / on-disk
// size -- the headline number for footprint-at-scale work. An optional
// heap profile attributes the resident bytes to the structures holding
// them.
//
// This is a MEMORY tool, not a timing benchmark: it prints no wall-clock
// perf numbers. The load still touches a large graph, so run it under the
// local-cpu lock on the shared box.
//
//	go run ./cmd/rssbench -graph ~/rustychickpeas-ldbc/export/sf1_canonical.rcpg
//	go run ./cmd/rssbench -graph <path> -memprofile inuse.pb.gz
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"

	chickpeas "github.com/freeeve/gochickpeas"
)

func main() {
	graph := flag.String("graph", "", "path to a .rcpg graph to load (required)")
	memProfile := flag.String("memprofile", "", "write an inuse_space heap profile after loading")
	flag.Parse()
	if *graph == "" {
		fmt.Fprintln(os.Stderr, "usage: rssbench -graph <path.rcpg> [-memprofile out.pb.gz]")
		os.Exit(2)
	}

	fi, err := os.Stat(*graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat %s: %v\n", *graph, err)
		os.Exit(1)
	}
	onDisk := fi.Size()

	rssBefore := processRSS()
	g, err := chickpeas.ReadRCPGFile(*graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", *graph, err)
		os.Exit(1)
	}

	// Settle: two GCs plus a return-to-OS so the reported RSS reflects the
	// retained working set, not transient load-time garbage still mapped.
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	rssAfter := processRSS()

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create memprofile: %v\n", err)
			os.Exit(1)
		}
		if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
			fmt.Fprintf(os.Stderr, "write memprofile: %v\n", err)
			os.Exit(1)
		}
		f.Close()
	}

	// The real footprint the runtime knows: everything it holds from the OS
	// minus what it has released. On macOS `ps rss` overcounts because
	// FreeOSMemory's MADV_FREE pages stay resident until reclaimed under
	// pressure -- Sys-HeapReleased excludes them, matching what Linux RSS
	// (and macOS phys_footprint) report. Portable, no OS tools needed.
	realFootprint := int64(m.Sys) - int64(m.HeapReleased)
	physFootprint := processPhysFootprint() // macOS kernel metric; 0 elsewhere

	mb := func(b int64) float64 { return float64(b) / (1 << 20) }
	umb := func(b uint64) float64 { return float64(b) / (1 << 20) }
	fmt.Printf("graph:            %s\n", *graph)
	fmt.Printf("on-disk:          %.1f MB\n", mb(onDisk))
	fmt.Printf("real footprint:   %.1f MB  (%.2fx on-disk; Sys - HeapReleased, macOS-accurate)\n",
		mb(realFootprint), float64(realFootprint)/float64(onDisk))
	if physFootprint > 0 {
		fmt.Printf("phys_footprint:   %.1f MB  (%.2fx on-disk; macOS kernel metric via vmmap)\n",
			mb(physFootprint), float64(physFootprint)/float64(onDisk))
	}
	if rssBefore > 0 && rssAfter > 0 {
		fmt.Printf("ps rss:           %.1f MB  (%.2fx; INFLATED on macOS -- counts MADV_FREE'd pages)\n",
			mb(rssAfter), float64(rssAfter)/float64(onDisk))
	}
	fmt.Printf("go HeapInuse:     %.1f MB\n", umb(m.HeapInuse))
	fmt.Printf("go HeapSys:       %.1f MB  (HeapIdle %.1f MB, released %.1f MB)\n",
		umb(m.HeapSys), umb(m.HeapIdle), umb(m.HeapReleased))
	fmt.Printf("go StackInuse:    %.1f MB\n", umb(m.StackInuse))
	fmt.Printf("go Sys (total):   %.1f MB\n", umb(m.Sys))
	// Keep g alive across the measurement so its memory is not collected.
	runtime.KeepAlive(g)
}

// processRSS returns this process's resident set size in bytes via
// `ps -o rss=` (KB on both macOS and Linux), or 0 when ps is unavailable.
func processRSS() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// processPhysFootprint returns the macOS kernel's phys_footprint for this
// process in bytes, parsed from `vmmap --summary`. This is the metric the
// kernel uses for memory pressure/jetsam and Activity Monitor's "Memory"
// column: it excludes MADV_FREE'd (reclaimable) pages that ps rss still
// counts, so it is the honest resident number on macOS. Returns 0 on
// non-Darwin or when vmmap is unavailable.
func processPhysFootprint() int64 {
	if runtime.GOOS != "darwin" {
		return 0
	}
	out, err := exec.Command("vmmap", "--summary", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Physical footprint:") {
			continue
		}
		field := strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
		return parseHumanBytes(field)
	}
	return 0
}

// parseHumanBytes converts a vmmap size token like "742.3M", "1.9G", or
// "512K" to bytes, or 0 if it cannot be parsed.
func parseHumanBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := float64(1)
	switch s[len(s)-1] {
	case 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(v * mult)
}
