# 011 -- gql scaffold (M10)

Port foundations for the GQL engine (from rustychickpeas-cypher, per the approved
plan in this session):

- `gql/value`: runtime Value (Null/Bool/Int/Float/Str/Node/Rel/Path/List/Map/
  Temporal/Duration), equality/compare/orderCmp/Kleene logic, byte-encoded GroupKey.
- `gql/errors.go`: ErrParse/ErrBind/ErrPlan sentinels.
- `gql/rows.go`: Row/Rows result surface.
- `gql/internal/graph`: Graph interface (portable read surface), *chickpeas.Snapshot
  adapter, pre-resolved NodeMatcher/RelMatcher.
- DESIGN.md: cypher/ reserved path becomes gql/, seam section updated.

Gate: value semantics tables (null ordering, f64 bits, list/map compare) match
Rust `value.rs`; adapter tests against a built Snapshot; `go test ./... -race`.
