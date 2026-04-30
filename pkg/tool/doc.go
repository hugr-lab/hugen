// Package tool implements the tool subsystem: Tool /
// ToolProvider contracts, a ToolManager with per-Turn snapshots
// keyed by (skill_gen, tool_gen, policy_gen), the system-tools
// provider (notepad_append, skill_*, runtime_reload, mcp_*,
// policy_*), and an MCP-client provider that wraps
// mark3labs/mcp-go.
//
// Every dispatch traverses three permission tiers: Tier 1
// (operator config floor) and Tier 2 (Hugr role rules) via
// pkg/auth/perm.Service, then Tier 3 (per-user tool_policies)
// via Policies.Decide. Tier 1 / Tier 2 Disabled is final — Tier 3
// cannot relax it. Permission denials surface as
// tool_error{code:"permission_denied"} plus a
// system_marker{subject:"tool_denied"} audit Frame.
//
// Tool catalogue is per-Turn: in-flight calls finish against the
// snapshot they started with; the next Turn rebuilds when the
// generation token changed.
package tool
