# 036: unify the removal surface's miss reporting (bool vs error)

Review of the 032 removal surface found the family splits its convention
with no documented rationale:

- `RemoveProp` (removal.go:61) and `RemoveNode` (:147) return bool for a
  missing target.
- `RemoveRelProp` (:89), `RemoveRelPropAt` (:101), `RemoveRel` (:123) return
  error (ErrRelNotFound) for the same not-found case.

Additional asymmetry: `RemoveProp` returns false when the key was never
interned (:63-65), conflating "no such key" with "nothing removed", while
`RemoveRelPropAt` treats an unstaged key as success/nil (:106-108) and errors
only on a missing rel. Callers cannot handle removals uniformly -- a no-op
rel-prop removal is indistinguishable from a real one.

Pick one convention for the whole family (suggest: `(removed bool, err error)`
only where a real failure mode exists, plain bool elsewhere; or error
everywhere with ErrNotFound sentinels), document the miss semantics on each
method, and align the tests (removal_test.go, example_thaw_test.go).

Rust mirror: if the rustychickpeas removal surface (tasks/223 there) copies
this API, file the same unification there per the cross-repo norm (task file
in that repo, left uncommitted).

## Resolution (2026-07-05)

Convention adopted (documented in removal.go's header): every method
reports whether staged state changed as a bool; error is reserved for a
dangling rel handle (index or (u,v,type) address that is out of range,
tombstoned, or dead via detach-delete -> ErrRelNotFound). Node ids are
open-world, so an unknown node is a plain miss, never an error.

- RemoveProp(node, key) bool -- unchanged signature; doc now states false
  covers every miss uniformly (never-interned key included: no staged pair
  could exist, so "no such key" and "nothing removed" coincide).
- RemoveNode(id) bool -- unchanged.
- RemoveRel(idx) error -- unchanged (a live handle always removes, so a
  bool would be redundant: removed <=> err == nil).
- RemoveRelProp / RemoveRelPropAt -> (removed bool, err error): the sweep
  result is now reported; an unstaged key on a live rel is (false, nil),
  distinguishable from both a real removal and a bad handle.

Tests updated + strengthened: removal_test.go asserts the reported bool on
real removals and both no-op flavors; fuzz_removal_test.go case 5 now
asserts removed == model-had-key on live rels (30s fuzz clean).

Rust mirror: their tasks/223 is still an unbuilt spec -- appended a dated
update section with this convention (uncommitted, cross-repo norm).
