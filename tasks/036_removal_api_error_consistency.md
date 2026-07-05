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
