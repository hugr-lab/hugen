package tool

import "context"

// ProviderBuilder turns a runtime-side Spec into a live
// ToolProvider. The builder is the only place that knows how to
// construct providers from raw spec fields — pkg/tool itself stays
// out of the construction business.
//
// The concrete builder lives in pkg/tool/providers; pkg/tool only
// declares the contract so ToolManager can hold a reference and
// route Spec values without importing the construction code.
//
// Builders are stateless from ToolManager's point of view —
// instances are constructed at boot (pkg/runtime) with their own
// dependencies (auth.Service, workspace root, logger) and passed
// in. ToolManager calls Build once per registered Spec.
type ProviderBuilder interface {
	Build(ctx context.Context, spec Spec) (ToolProvider, error)
}
