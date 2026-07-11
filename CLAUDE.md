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
