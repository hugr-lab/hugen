// Package config exposes the runtime configuration aggregate as
// per-domain views. Downstream packages depend on a narrow View
// interface (LocalView, ModelsView, ToolProvidersView,
// PermissionsView, AuthView, EmbeddingView), not on the full
// aggregate, so each constructor's surface stays auditable.
//
// Phase 3 ships a static loader (NewStaticService) — config is
// read once at boot and OnUpdate callbacks never fire. The live
// (fs-watch + Hugr-subscription) implementation arrives in
// phase 6+.
package config
