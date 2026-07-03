// PlanCache: a size-bounded LRU cache of auto-parameterized query plans.
// Two layers share one lock. An exact-string fast path (L1) skips parse +
// plan on a verbatim repeat; a template index (L2) lets queries differing
// only in lifted constants share one compiled plan. L1 entries own the
// plans; a template entry dies when its last L1 variant is evicted (the Go
// refcount standing in for the Rust engine's Arc/Weak split).
//
// Both layers hold the TEMPLATE plan -- planned after auto-
// parameterization, where every cost probe abstains on parameter slots --
// so a cached plan is value-independent by construction. It may differ in
// SHAPE (never in results) from the literal-probed plan an uncached Run
// builds for the same text; this mirrors the Rust engine's query_cached
// composition, trading the best-effort literal plan for zero double
// planning and cross-literal sharing.
//
// Eviction is least-recently-used past a configurable byte budget. The
// byte estimate counts each L1 entry's key, parameters, and a per-plan
// estimate; a plan shared by several literal variants is charged once per
// variant -- a deliberate over-count, so the true footprint stays under
// the bound.
package gql

import (
	"sort"
	"sync"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
	"github.com/freeeve/gochickpeas/gql/value"
)

// DefaultCacheBytes is the default budget: 128 MiB of approximate
// resident plan + key + parameter bytes.
const DefaultCacheBytes = 128 << 20

// l1Overhead is the per-entry fixed bookkeeping charge.
const l1Overhead = 96

// cachedPlan is one template's compiled plan, shared across every L1
// literal variant of that template.
type cachedPlan struct {
	plan     *plan.Plan
	mode     ast.QueryMode
	key      string
	estBytes int
	// refs counts the L1 variants sharing this plan; the template entry
	// is dropped when the last one evicts.
	refs int
}

// l1Entry is one verbatim query string's cache slot: the shared plan plus
// this string's lifted constant values.
type l1Entry struct {
	plan   *cachedPlan
	params []value.Value
	bytes  int
	tick   uint64
}

// PlanCache is a size-bounded cache of auto-parameterized plans, safe for
// concurrent use. Hold one per long-lived snapshot: a cached plan's cost
// choices reflect the statistics of the snapshot its template was first
// planned against (executing against another snapshot is correct, possibly
// suboptimal).
type PlanCache struct {
	mu         sync.Mutex
	byQuery    map[string]*l1Entry
	byTemplate map[string]*cachedPlan
	bytes      int
	tick       uint64
	maxBytes   int

	hitsL1 uint64
	hitsL2 uint64
	misses uint64
}

// NewPlanCache is an empty cache bounded to maxBytes of approximate
// resident memory (LRU eviction past the bound); maxBytes <= 0 means
// DefaultCacheBytes.
func NewPlanCache(maxBytes int) *PlanCache {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &PlanCache{
		byQuery:    map[string]*l1Entry{},
		byTemplate: map[string]*cachedPlan{},
		maxBytes:   maxBytes,
	}
}

// Run executes a GQL query through the cache.
func (c *PlanCache) Run(g *chickpeas.Snapshot, query string) (*Rows, error) {
	return c.RunWithParams(g, query, nil)
}

// RunWithParams executes a GQL query through the cache with explicit
// $name parameter values. Results are identical to the uncached
// RunWithParams.
func (c *PlanCache) RunWithParams(g *chickpeas.Snapshot, query string, params map[string]value.Value) (*Rows, error) {
	gr := graph.New(g)
	// L1: a verbatim repeat skips parse + plan.
	if cp, lifted, ok := c.l1Lookup(query); ok {
		return c.execCached(gr, cp, lifted, params)
	}
	// L2: parse, lift constants, and share a plan across templates.
	q, err := parseDesugar(query)
	if err != nil {
		return nil, err
	}
	lifted := semantics.AutoParameterize(q)
	key := ast.Fingerprint(q)
	cp, ok := c.l2Lookup(key)
	if !ok {
		// Plan the template outside the lock; a concurrent duplicate is
		// reconciled by insert (the first plan wins, the second is
		// dropped).
		p, err := plan.Build(q, gr)
		if err != nil {
			return nil, wrapStage(err)
		}
		cp = &cachedPlan{plan: p, mode: q.Mode, key: key, estBytes: 2048 + len(key)*8}
	}
	c.insert(query, cp, lifted)
	return c.execCached(gr, cp, lifted, params)
}

// execCached executes a cached plan with this call's lifted values and
// named parameters (cached EXPLAIN/PROFILE renders show no planning time,
// matching the Rust engine).
func (c *PlanCache) execCached(gr *graph.SnapshotGraph, cp *cachedPlan, lifted []value.Value, named map[string]value.Value) (*Rows, error) {
	ctx := &eval.Ctx{G: gr, Params: lifted, Named: named, ForceInterp: forceInterp}
	return execPlan(gr, cp.plan, cp.mode, 0, ctx)
}

// l1Lookup returns the verbatim entry's shared plan and lifted values,
// marking it most-recently-used.
func (c *PlanCache) l1Lookup(query string) (*cachedPlan, []value.Value, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byQuery[query]
	if !ok {
		return nil, nil, false
	}
	c.tick++
	e.tick = c.tick
	c.hitsL1++
	return e.plan, e.params, true
}

// l2Lookup returns an existing template's plan, counting the hit.
func (c *PlanCache) l2Lookup(key string) (*cachedPlan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp, ok := c.byTemplate[key]
	if ok {
		c.hitsL2++
	} else {
		c.misses++
	}
	return cp, ok
}

// insert memoizes the exact query string (L1) against the template's
// shared plan (L2), reconciling a concurrently inserted duplicate
// template, then evicts LRU entries past the budget.
func (c *PlanCache) insert(query string, cp *cachedPlan, lifted []value.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.byTemplate[cp.key]; ok {
		cp = existing
	} else {
		c.byTemplate[cp.key] = cp
	}
	c.tick++
	bytes := l1Overhead + len(query) + cp.estBytes
	for _, v := range lifted {
		bytes += valueBytes(v)
	}
	if old, ok := c.byQuery[query]; ok {
		c.bytes -= old.bytes
		c.deref(old.plan)
	}
	cp.refs++
	c.byQuery[query] = &l1Entry{plan: cp, params: lifted, bytes: bytes, tick: c.tick}
	c.bytes += bytes
	c.evict()
}

// deref drops one L1 variant's hold; the template entry dies with the
// last one.
func (c *PlanCache) deref(cp *cachedPlan) {
	cp.refs--
	if cp.refs <= 0 {
		delete(c.byTemplate, cp.key)
	}
}

// evict removes least-recently-used L1 entries until resident bytes fall
// to 90% of the budget (hysteresis: a steady insert stream doesn't evict
// on every call). Caller holds the lock.
func (c *PlanCache) evict() {
	if c.bytes <= c.maxBytes {
		return
	}
	target := c.maxBytes / 10 * 9
	type tk struct {
		tick uint64
		key  string
	}
	order := make([]tk, 0, len(c.byQuery))
	for k, e := range c.byQuery {
		order = append(order, tk{e.tick, k})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].tick < order[j].tick })
	for _, o := range order {
		if c.bytes <= target {
			break
		}
		e := c.byQuery[o.key]
		delete(c.byQuery, o.key)
		c.bytes -= e.bytes
		c.deref(e.plan)
	}
}

// valueBytes approximates one lifted value's heap footprint.
func valueBytes(v value.Value) int {
	switch v.Kind() {
	case value.KindStr:
		s, _ := v.AsStr()
		return len(s) + 24
	case value.KindList:
		xs, _ := v.AsList()
		n := 24
		for _, x := range xs {
			n += valueBytes(x)
		}
		return n
	case value.KindPath:
		ns, rs, _ := v.AsPath()
		return 24 + len(ns)*4 + len(rs)*4
	default:
		return 16
	}
}

// Bytes is the approximate resident bytes currently charged.
func (c *PlanCache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// MaxBytes is the current byte budget.
func (c *PlanCache) MaxBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxBytes
}

// SetMaxBytes changes the budget, evicting immediately if over it.
func (c *PlanCache) SetMaxBytes(maxBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = maxBytes
	c.evict()
}

// Len is the number of distinct templates with a live plan.
func (c *PlanCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byTemplate)
}

// IsEmpty reports whether no live template plan is cached.
func (c *PlanCache) IsEmpty() bool { return c.Len() == 0 }

// stats exposes the hit/miss counters to same-package tests.
func (c *PlanCache) stats() (l1, l2, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hitsL1, c.hitsL2, c.misses
}
