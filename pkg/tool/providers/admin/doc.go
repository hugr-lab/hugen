// Package admin hosts the type-agnostic registry-administration
// ToolProvider — `tool:provider_add` / `tool:provider_remove`.
//
// AdminProvider replaces the legacy SystemProvider's mcp_add /
// mcp_remove tools. It is type-agnostic — Spec.Type is dispatched
// through the ToolManager's wired ProviderBuilder, so any provider
// type the Builder supports (today: mcp; future: webhook,
// kubernetes) is reachable via `tool:provider_add` without
// expanding the tool surface.
//
// Imports: pkg/tool only.
package admin
