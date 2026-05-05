// Package tool defines the contract surface for the tool
// subsystem: Tool / ToolProvider / Recoverable interfaces, a
// generic ToolManager dispatcher, the PolicyService interface
// consulted on Resolve, and the cross-cutting Spec /
// ProviderBuilder shapes that wire concrete provider types.
//
// pkg/tool intentionally has no provider implementations of its
// own. MCP, recovery, admin, policies, and runtime:reload all
// live under pkg/tool/providers/*; pkg/runtime + pkg/session
// construct them and register through ToolManager.
//
// Every dispatch traverses three permission tiers: Tier 1
// (operator config floor) and Tier 2 (Hugr role rules) via
// pkg/auth/perm.Service, then Tier 3 (per-user tool_policies)
// via PolicyService.Decide. Tier 1 / Tier 2 Disabled is final —
// Tier 3 cannot relax it. Permission denials surface as
// tool_error{code:"permission_denied"} plus a
// system_marker{subject:"tool_denied"} audit Frame.
//
// Recovery is lazy and out-of-band: the Recoverable interface is
// consumed by pkg/tool/providers/recovery.Wrap, which decorates
// failure-prone providers (today: MCP) and re-issues the failed
// Call/List after TryReconnect rebuilds the underlying client.
// pkg/tool itself runs no goroutines, holds no recovery state,
// and dispatches calls directly to whatever provider was
// registered.
//
// Tool catalogue is per-Turn: in-flight calls finish against the
// snapshot they started with; the next Turn rebuilds when the
// generation token changed.
package tool
