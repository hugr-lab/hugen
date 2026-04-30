package config

import "context"

// Service is the consumer-facing aggregate. Each domain takes
// only the View it needs; tests construct fakes by implementing
// the relevant View interface directly.
type Service interface {
	Local() LocalView
	Models() ModelsView
	Embedding() EmbeddingView
	Auth() AuthView
	Permissions() PermissionsView
	ToolProviders() ToolProvidersView

	// Subscribe streams coarse "config changed" events for any
	// caller that wants a single signal across all domains. Per-
	// domain consumers prefer View.OnUpdate. Phase 3 static
	// service never fires.
	Subscribe(ctx context.Context) (<-chan ConfigEvent, error)
}

// ConfigEvent is the coarse change signal Subscribe emits. Phase
// 3 static service does not emit; phase 6+ live reload will fire
// one event per snapshot diff with Domains naming the affected
// views.
type ConfigEvent struct {
	Domains []string // e.g. ["permissions"], ["auth", "tool_providers"]
}
