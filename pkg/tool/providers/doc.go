// Package providers hosts the concrete tool.ProviderBuilder
// implementation that ToolManager dispatches Spec values through.
//
// Phase 4.1a stage A step 4 introduces this package: Builder
// switches on Spec.Type and delegates to the type-specific
// subpackage (today: pkg/tool/providers/mcp). Adding a new
// provider type is two lines — import the new subpackage and add
// a case in the switch — without touching pkg/tool itself.
//
// Imports: pkg/auth, pkg/auth/perm, pkg/tool, pkg/tool/providers/mcp
// (and future siblings). Sibling subpackages do NOT import this
// package; the dispatch fan-out is one-way.
package providers
