package tool

import "context"

// MCPLifecycle is the contract Reconnector consumes. The concrete
// implementation lives in pkg/tool/providers/mcp; pkg/tool depends
// only on the interface so the manager package stays free of the
// mcp-go client surface and the auth-injection helpers
// (pkg/auth, mark3labs/mcp-go).
//
// Reconnectable extends it with the SetStaleHook setter
// ToolManager.AddProvider wires to Reconnector.Track. Providers
// implement Reconnectable when they want to participate in the
// background recovery loop; non-MCP providers (admin, policies,
// runtime:reload, session:*) stay out of the loop.
type MCPLifecycle interface {
	// Name returns the provider's short name; must match the prefix
	// of every Tool.Name the provider exposes.
	Name() string
	// IsClosed reports whether the provider has been Close()'d. The
	// Reconnector untracks closed providers on the next tick.
	IsClosed() bool
	// IsStale reports whether the underlying client is gone pending
	// a successful Reconnect.
	IsStale() bool
	// Reconnect rebuilds the underlying client. Called by the
	// Reconnector loop on every backoff tick.
	Reconnect(ctx context.Context) error
}

// Reconnectable is the surface ToolManager.AddProvider type-asserts
// to wire the provider's stale-hook to the manager's Reconnector.
// Implementations call the supplied func(MCPLifecycle) once on each
// healthy → stale transition.
type Reconnectable interface {
	MCPLifecycle
	SetStaleHook(func(MCPLifecycle))
}
