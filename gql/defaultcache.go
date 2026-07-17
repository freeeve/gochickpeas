// The implicit per-snapshot plan cache behind Run/RunWithParams: library
// callers get template caching (skip parse + plan on repeats) with no
// setup, per the census verdict -- every measured flip is timing-neutral
// and the flip-aware bypass routes structurally flipped templates through
// sighted planning, so the cached path does no harm. The registry keys
// caches by WEAK snapshot pointers with a GC cleanup, so dropping a
// snapshot drops its cache; a cache never outlives (or pins) its
// snapshot. Callers wanting the literal-planned path explicitly --
// benchmarks separating cold from warm, plan-shape experiments that
// mutate planner thresholds between runs -- use RunUncached.
package gql

import (
	"runtime"
	"sync"
	"weak"

	chickpeas "github.com/freeeve/gochickpeas"
)

var (
	defaultCachesMu sync.Mutex
	defaultCaches   = map[weak.Pointer[chickpeas.Snapshot]]*PlanCache{}
)

// DefaultCacheFor returns the snapshot's implicit plan cache, creating it
// on first use (DefaultCacheBytes budget). Run and RunWithParams route
// through it; sharing the same cache with explicit PlanCache calls is
// fine but unnecessary.
func DefaultCacheFor(g *chickpeas.Snapshot) *PlanCache {
	wp := weak.Make(g)
	defaultCachesMu.Lock()
	defer defaultCachesMu.Unlock()
	if c, ok := defaultCaches[wp]; ok {
		return c
	}
	c := NewPlanCache(0)
	defaultCaches[wp] = c
	runtime.AddCleanup(g, func(key weak.Pointer[chickpeas.Snapshot]) {
		defaultCachesMu.Lock()
		delete(defaultCaches, key)
		defaultCachesMu.Unlock()
	}, wp)
	return c
}
