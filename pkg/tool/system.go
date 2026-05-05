package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// systemProviderName is the prefix every Tool exposed by
// SystemProvider carries. Phase 4.1a steps 20-25 thinned this
// provider down to zero tools — every legacy entry migrated to a
// dedicated provider:
//
//   - skill_*, notepad_append, tool_catalog → session.Manager
//   - policy_save / policy_revoke → policies.Policies
//   - mcp_*_server → admin.AdminProvider + runtime:reload(target=mcp)
//   - runtime_reload → runtime.ReloadProvider
//
// Step 27 deletes the type entirely. The skeleton stays here for
// the duration of the shim so the type assertions in cmd/hugen
// (still using legacy SystemDeps wiring) keep compiling.
const (
	systemProviderName = "system"
)

// SystemDeps used to wire callbacks into SystemProvider. Every
// callback migrated to dedicated providers; the empty struct
// remains so cmd/hugen tests that still pass it compile until
// step 27 removes the type altogether.
type SystemDeps struct{}

// SystemProvider is now a no-op tool.ToolProvider — it lists no
// tools and rejects every Call with ErrUnknownTool. Kept alive
// until step 27 of phase 4.1a so the migration ordering stays
// reviewable.
type SystemProvider struct{}

// NewSystemProvider constructs the (empty) provider.
func NewSystemProvider(_ SystemDeps) *SystemProvider {
	return &SystemProvider{}
}

// ErrSystemUnavailable is retained as the well-known sentinel some
// pkg/session handlers still surface when a side dep is unset
// (e.g. session:skill_load with no SkillManager wired). The error
// outlives SystemProvider itself.
var ErrSystemUnavailable = errors.New("tool: system tool unavailable in this runtime")

func (p *SystemProvider) Name() string       { return systemProviderName }
func (p *SystemProvider) Lifetime() Lifetime { return LifetimePerAgent }

func (p *SystemProvider) List(context.Context) ([]Tool, error) {
	return nil, nil
}

func (p *SystemProvider) Call(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
}

func (p *SystemProvider) Subscribe(context.Context) (<-chan ProviderEvent, error) {
	return nil, nil
}

func (p *SystemProvider) Close() error { return nil }
