// Package mcp implements the MCP-protocol ToolProvider.
//
// One Provider struct owns the mark3labs/mcp-go client and
// implements both tool.ToolProvider and tool.MCPLifecycle so
// ToolManager can dispatch tool calls and Reconnector can drive
// background recovery. Phase 4.1c retired the Inner-wrapper
// pattern that previously wrapped a legacy pkg/tool.MCPProvider —
// the implementation now lives natively here.
//
// Two construction entry points coexist:
//
//   - New(ctx, tool.Spec, authSvc, workspaceRoot, log) — runtime
//     entry consumed by providers.Builder. Folds auth injection
//     (HTTP RoundTripper or stdio bootstrap mint) and stdio
//     workspace-root injection in via the unexported buildSpec.
//   - NewWithSpec(ctx, Spec, log) — direct entry for tests and
//     pkg/session.Resources.Acquire (per-session bash-mcp etc.).
//
// Imports: pkg/auth, pkg/auth/perm, pkg/config, pkg/tool,
// mark3labs/mcp-go.
package mcp
