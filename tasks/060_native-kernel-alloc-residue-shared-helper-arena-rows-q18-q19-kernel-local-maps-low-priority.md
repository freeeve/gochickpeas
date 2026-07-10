# 060 -- native kernel alloc residue: shared-helper arena rows + Q18/Q19 kernel-local maps (low priority)

Opened 2026-07-10, carved out of tasks/028's close-out as its remaining
(deprioritized) residue. Native suite at dc7de70: 2,267,750 allocs.

Two distinct shapes, from the published profile:

1. **Row-proportional output materialization** -- SPB a13 1.0M allocs for
   336k rows (3.0/row), a5 218k (2.0/row), a25 190k (4.0/row): the
   [][]int64 / [][]any result rows the shared helpers (aggRows etc.)
   build one make per row. The same materialization the Rust side's
   counting allocator reports, so the cross-engine comparison is fair as
   is; an arena-row variant of the SHARED helpers (never per-kernel
   tweaks -- kernels are readable reference ports) would reduce it if
   absolute numbers ever matter.
2. **Kernel-local maps** -- BI/Q18 199k allocs for 20 rows, BI/Q19 51k
   for 6: per-run scratch maps inside two kernels. Q18-native still runs
   ~29 ms, so this is cosmetic today. Only touch with a general scratch
   mechanism if these kernels ever rank in a timing profile.

Not urgent by 028's own assessment; take this up only if the ldbc side
files evidence that native absolute allocs matter for a comparison.
