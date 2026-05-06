// Package mcp implements the MCP-protocol ToolProvider.
//
// One Provider struct owns the mark3labs/mcp-go client and
// implements both tool.ToolProvider (so ToolManager can dispatch
// tool calls) and tool.Recoverable (so pkg/tool/providers/recovery
// can rebuild the client on failure). There is no background
// goroutine, no central scheduler, no callbacks — recovery is
// lazy: a failed Call/List propagates upstream through the
// recovery wrapper, which then walks a backoff schedule calling
// Provider.TryReconnect between attempts.
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
