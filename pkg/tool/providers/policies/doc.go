// Package policies hosts Tier-3 personal-policy storage and the
// `policy:save` / `policy:revoke` tool surface.
//
// One Policies struct implements both tool.PolicyService (consulted
// by ToolManager.Resolve) and tool.ToolProvider (exposes the LLM
// tools). The struct owns its types.Querier directly — phase 4.1c
// retired the legacy *tool.Policies wrapper that used to live in
// pkg/tool, closing the pkg/store/queries import gate on pkg/tool.
//
// pkg/runtime constructs Policies once per agent against the local
// store and registers it via ToolManager.SetPolicies +
// ToolManager.AddProvider; deployments without a local store skip
// the wiring and Tier-3 stays disabled (Decide returns PolicyAsk,
// IsConfigured false → policy:save / policy:revoke surface
// ErrSystemUnavailable).
//
// Imports: pkg/auth/perm, pkg/store/queries, pkg/tool,
// query-engine/types.
package policies
