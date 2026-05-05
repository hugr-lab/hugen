// Package mcp implements the MCP-protocol ToolProvider.
//
// Phase 4.1a stage A step 3 introduces this subpackage as the
// new home for MCP-aware provider construction; subsequent
// stages migrate callers off the legacy pkg/tool.MCPProvider /
// tool.NewMCPProvider entry points and finally relocate the
// implementation here. During the interim the package wraps
// the existing *tool.MCPProvider — its onClose slice owns
// revoke callbacks (replacing the manager-side cleanups map),
// and Inner() exposes the underlying provider so external
// integration (stale-hook wiring) can reach it.
//
// Imports: pkg/auth, pkg/config, pkg/tool. Does NOT import
// sibling packages under pkg/tool/providers.
package mcp
