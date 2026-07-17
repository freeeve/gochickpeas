# gochickpeas -- engine guidelines

## Performance work: generalized wins only, no query overfitting

The single most important constraint when optimizing this engine: **every
optimization must be a general engine improvement, never a recognizer for a
specific benchmark query.**

Concretely, the following is forbidden:

- Fingerprinting or AST-pattern-matching a known query (IC3, Q6, CR1, ...)
  and dispatching it to a hand-written kernel or a bespoke code path.
- Any branch whose condition is "this looks like benchmark query X".
- Precomputing or caching a result that only the benchmark's exact
  parameters would hit.

(The Rust sibling repo regressed into exactly this -- recognizing queries
and translating them to kernel calls -- which inflates benchmark numbers
without improving the engine. Do not repeat it here.)

What IS allowed and encouraged -- optimizations that fire on generic
structure, not on query identity:

- Representation choices keyed on runtime **value kind** (e.g. a uint32
  entity-id set for DISTINCT over nodes/rels, vs a byte-string key for other
  kinds). This mirrors what a real query engine does per column type.
- Buffer / scratch reuse across rows, iterator and closure-escape removal,
  set/map recycling, shape caches keyed on the pattern (not the query).
- Core API changes when a clear general win needs them (core has no external
  consumers yet), as long as the surface stays GQL-conformant.

The test: if a change would help an unseen query with the same shape, it is
general. If it only helps because the engine "knows" this is query X, it is
overfitting -- do not land it.

Every optimization round ends with the parity gate green (currently 78/78
gql MATCH, 89/89 native; the manifest grows as the ldbc session authors
queries) -- the gate is what proves a fast path never diverges from the
general path.

## Backed-out experiments go to a research branch, not the bit bucket

When an optimization is **correct and general but shows no measured win**
(or a win the current box/workload can't confirm), don't `git checkout`
it away -- preserve it on a pushed `research/<name>` branch off the commit
it was built against. Many such changes are not wrong, only unconfirmable
here-and-now: a macOS profile mislocated the cost, the shape is
bandwidth-bound so the CPU-side change can't show, or the change only pays
once a larger piece (batch execution, a different anchor) lands and
composes with it. The branch commit message must record what it does, that
it is parity-green, the measured (non-)result, and the revisit conditions.
A change that actively REGRESSES stays out of the tree too, but its design
belongs in the task ledger; a change that is merely unproven belongs on a
branch where a later session can pick it up. (Precedent: research/proj-
slot-gather, the Q11 projection slot-gather -- parity-green, no wall win,
macOS-profile-artifact motivation.) This composes with the measurement
discipline below: measure first, and when the verdict is "correct but
flat," branch rather than discard.

## Allocation work: consult and extend the strategy catalog

`docs/zero_alloc_target.md` is the running catalog of generalized Go
allocation-reduction strategies (flat probe tables, hoist+reset scratch,
per-worker accumulators, batch seams over iterator closures, typed rows,
constant memoization, ...), each with the repo commit that proved it.
Before an allocation pass, read it; when a NEW technique lands (or an
existing one is refined by a surprising measurement), add or update the
entry with the proving commit. The measurement discipline at its top
(MemProfileRate=1 profile first, warm-run counts, stepwise A/B,
result-identity gate) is mandatory for any alloc change.

## Share insights with rustychickpeas when applicable

When an optimization or technique lands here that plausibly ports to the
Rust sibling (rustychickpeas), file a cross-project ask in its ledger
(`taskman file rustychickpeas "..."`) describing the technique, the
measured numbers, where the reference implementation lives here, and the
verification pattern that made it safe. The two engines share the .rcpg
format and the ldbc benchmark suite; wins should flow both directions
(precedent: task 264, the load-throughput techniques that took Go past
the Rust floor). Never edit the sibling's code directly -- the ledger ask
is the channel.
