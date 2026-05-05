// Package policies hosts Tier-3 personal-policy storage and the
// `policy:save` / `policy:revoke` tool surface.
//
// Phase 4.1a stage A step 5 introduces this subpackage as the
// new home for Policies; the legacy *tool.Policies stays in
// pkg/tool until stage A step 7 dissolves the deps. During the
// interim, *Policies in this package wraps the legacy type via
// composition — Inner() exposes it for callers (today:
// ToolManager.SetPolicies) that still expect the legacy shape.
//
// The Store interface (store.go) decouples Policies from the
// persistence layer; the DuckDB-backed implementation lives in
// pkg/store/queries and is wired by pkg/runtime once the rest
// of the refactor lands.
//
// Imports: pkg/auth/perm, pkg/tool. Does NOT import pkg/config
// or pkg/store directly — the boundary is the Store interface.
package policies
