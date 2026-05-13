package config

import (
	"context"
	"time"
)

// Compile-time assertion: *StaticService satisfies every View
// and the Service aggregate.
var (
	_ Service           = (*StaticService)(nil)
	_ LocalView         = (*StaticService)(nil)
	_ ModelsView        = (*StaticService)(nil)
	_ EmbeddingView     = (*StaticService)(nil)
	_ AuthView          = (*StaticService)(nil)
	_ PermissionsView   = (*StaticService)(nil)
	_ ToolProvidersView = (*StaticService)(nil)
	_ SubagentsView     = (*StaticService)(nil)
)

// Subagent runtime defaults — applied in NewStaticService when the
// caller omits a SubagentsConfig field. Numbers anchor to phase-4-spec
// §3 step 10 (and phase-5.1 § 4.5 for MaxAsyncMissionsPerRoot).
const (
	defaultSubagentMaxDepth                = 5
	defaultSubagentMaxTurns                = 15
	defaultStuckRepeatedHash               = 3
	defaultStuckTightDensityCount          = 3
	defaultStuckTightDensityWindow         = 2 * time.Second
	defaultSubagentMaxAsyncMissionsPerRoot = 5
)

// HITL runtime defaults — applied in NewStaticService when the
// caller omits a HitlConfig field. Phase-5.1 § 2.7.
const (
	defaultHitlInquireTimeoutMs = 60 * 60 * 1000 // 1 hour
)

// StaticService is the phase-3 implementation of Service. It is
// load-once: snapshots are populated at NewStaticService time and
// never change. OnUpdate callbacks are stored but never invoked.
// Subscribe returns a never-firing channel.
//
// The struct itself satisfies every View interface; Service
// methods all return the same pointer cast to the relevant view.
type StaticService struct {
	localDB        LocalConfig
	localDBEnabled bool
	models         ModelsConfig
	embedding      EmbeddingConfig
	auth           []AuthSource
	permissions    []PermissionRule
	permSettings   PermissionSettings
	toolProviders  []ToolProviderSpec
	subagents      SubagentsConfig
	hitl           HitlConfig
}

// StaticInput aggregates everything NewStaticService needs from
// cmd/hugen. Pure data; no behaviour.
type StaticInput struct {
	LocalDB        LocalConfig
	LocalDBEnabled bool
	Models         ModelsConfig
	Embedding      EmbeddingConfig
	Auth           []AuthSource
	Permissions    []PermissionRule
	PermSettings   PermissionSettings
	ToolProviders  []ToolProviderSpec
	Subagents      SubagentsConfig
	Hitl           HitlConfig
}

// NewStaticService captures the input snapshot. The caller still
// owns the data; we copy slice headers but not their elements
// (they are value types or treated as immutable JSON blobs).
func NewStaticService(in StaticInput) *StaticService {
	if in.PermSettings.RefreshInterval <= 0 {
		in.PermSettings.RefreshInterval = 5 * time.Minute
	}
	if in.PermSettings.HardExpiry <= 0 {
		in.PermSettings.HardExpiry = 3 * in.PermSettings.RefreshInterval
	}
	subagents := in.Subagents
	if subagents.MaxDepth <= 0 {
		subagents.MaxDepth = defaultSubagentMaxDepth
	}
	if subagents.MaxTurns <= 0 {
		subagents.MaxTurns = defaultSubagentMaxTurns
	}
	if subagents.StuckDetection.RepeatedHash <= 0 {
		subagents.StuckDetection.RepeatedHash = defaultStuckRepeatedHash
	}
	if subagents.StuckDetection.TightDensityCount <= 0 {
		subagents.StuckDetection.TightDensityCount = defaultStuckTightDensityCount
	}
	if subagents.StuckDetection.TightDensityWindow <= 0 {
		subagents.StuckDetection.TightDensityWindow = defaultStuckTightDensityWindow
	}
	if subagents.MaxAsyncMissionsPerRoot <= 0 {
		subagents.MaxAsyncMissionsPerRoot = defaultSubagentMaxAsyncMissionsPerRoot
	}
	hitl := in.Hitl
	if hitl.DefaultTimeoutMs <= 0 {
		hitl.DefaultTimeoutMs = defaultHitlInquireTimeoutMs
	}
	return &StaticService{
		localDB:        in.LocalDB,
		localDBEnabled: in.LocalDBEnabled,
		models:         in.Models,
		embedding:      in.Embedding,
		auth:           append([]AuthSource(nil), in.Auth...),
		permissions:    append([]PermissionRule(nil), in.Permissions...),
		permSettings:   in.PermSettings,
		toolProviders:  append([]ToolProviderSpec(nil), in.ToolProviders...),
		subagents:      subagents,
		hitl:           hitl,
	}
}

// --- Service interface ---

func (s *StaticService) Local() LocalView                 { return s }
func (s *StaticService) Models() ModelsView               { return s }
func (s *StaticService) Embedding() EmbeddingView         { return s }
func (s *StaticService) Auth() AuthView                   { return s }
func (s *StaticService) Permissions() PermissionsView     { return s }
func (s *StaticService) ToolProviders() ToolProvidersView { return s }
func (s *StaticService) Subagents() SubagentsView         { return s }
func (s *StaticService) Hitl() HitlView                   { return s }

// Subscribe returns a never-firing, never-closed channel. Phase-3
// callers can wire it without special-casing; phase-6+ live
// reload will replace this with a real implementation.
func (s *StaticService) Subscribe(ctx context.Context) (<-chan ConfigEvent, error) {
	ch := make(chan ConfigEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// --- LocalView ---

func (s *StaticService) LocalDB() LocalConfig { return s.localDB }
func (s *StaticService) LocalDBEnabled() bool { return s.localDBEnabled }

// --- ModelsView ---

// ModelsConfig is the data accessor; the Models() method on Service
// returns the View interface, so the names don't collide.
func (s *StaticService) ModelsConfig() ModelsConfig { return s.models }

// --- EmbeddingView ---

func (s *StaticService) EmbeddingConfig() EmbeddingConfig { return s.embedding }

// --- AuthView ---

func (s *StaticService) Sources() []AuthSource {
	out := make([]AuthSource, len(s.auth))
	copy(out, s.auth)
	return out
}

// --- PermissionsView ---

func (s *StaticService) Rules() []PermissionRule {
	out := make([]PermissionRule, len(s.permissions))
	copy(out, s.permissions)
	return out
}

func (s *StaticService) RefreshInterval() time.Duration {
	return s.permSettings.RefreshInterval
}

func (s *StaticService) RemoteEnabled() bool { return s.permSettings.RemoteEnabled }

// --- ToolProvidersView ---

func (s *StaticService) Providers() []ToolProviderSpec {
	out := make([]ToolProviderSpec, len(s.toolProviders))
	copy(out, s.toolProviders)
	return out
}

// --- SubagentsView ---

func (s *StaticService) DefaultMaxDepth() int           { return s.subagents.MaxDepth }
func (s *StaticService) DefaultMaxTurns() int           { return s.subagents.MaxTurns }
func (s *StaticService) DefaultStuckDetection() StuckPolicy {
	return s.subagents.StuckDetection
}
func (s *StaticService) MaxAsyncMissionsPerRoot() int { return s.subagents.MaxAsyncMissionsPerRoot }

// --- HitlView ---

func (s *StaticService) DefaultTimeoutMs() int { return s.hitl.DefaultTimeoutMs }

// --- OnUpdate (shared no-op) ---

// OnUpdate satisfies every View interface's OnUpdate method. The
// static service never fires; the returned cancel is a no-op too.
func (s *StaticService) OnUpdate(fn func()) (cancel func()) {
	_ = fn
	return func() {}
}
