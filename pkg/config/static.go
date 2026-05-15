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
	defaultStuckRepeatedHash               = 3
	defaultStuckTightDensityCount          = 3
	defaultStuckTightDensityWindow         = 2 * time.Second
	defaultSubagentMaxAsyncMissionsPerRoot = 5
)

// Per-tier turn-loop defaults — applied in NewStaticService when
// the operator does not supply a `subagents.tier_defaults.<tier>`
// block. Phase 5.2 δ (B3 migration). Worker tier matches today's
// runtime constants (defaultMaxToolIterations=40, hard=80) so
// post-migration behaviour is byte-identical for worker sessions
// that don't carry per-role overrides. Root / mission shrink —
// they coordinate, not execute, and benefit from a tighter loop
// so a wedged routing turn surfaces faster.
const (
	defaultTierRootMaxTurns         = 12
	defaultTierRootMaxTurnsHard     = 24
	defaultTierMissionMaxTurns      = 16
	defaultTierMissionMaxTurnsHard  = 32
	defaultTierWorkerMaxTurns       = 40
	defaultTierWorkerMaxTurnsHard   = 80
	tierLabelRoot                   = "root"
	tierLabelMission                = "mission"
	tierLabelWorker                 = "worker"
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
	if subagents.MaxAsyncMissionsPerRoot <= 0 {
		subagents.MaxAsyncMissionsPerRoot = defaultSubagentMaxAsyncMissionsPerRoot
	}
	subagents.TierDefaults = mergeTierDefaults(subagents.TierDefaults)
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

func (s *StaticService) DefaultMaxDepth() int        { return s.subagents.MaxDepth }
func (s *StaticService) MaxAsyncMissionsPerRoot() int { return s.subagents.MaxAsyncMissionsPerRoot }

// TierDefaults returns a defensive copy of the per-tier turn-loop
// defaults map so callers may not mutate the static snapshot.
// Phase 5.2 δ.
func (s *StaticService) TierDefaults() map[string]TierTurnDefaults {
	if len(s.subagents.TierDefaults) == 0 {
		return nil
	}
	out := make(map[string]TierTurnDefaults, len(s.subagents.TierDefaults))
	for k, v := range s.subagents.TierDefaults {
		out[k] = v
	}
	return out
}

// mergeTierDefaults applies the runtime per-tier defaults on top
// of the operator-supplied values: any zero field on a tier entry
// falls back to the constant; tiers absent from the input map are
// materialised with the full runtime default. Phase 5.2 δ. Stuck-
// detection thresholds inherit defaults from the existing
// subagents-level constants so the same number lands on every
// tier (the runtime today reads these from constants — δ only
// plumbs the enable bit through; threshold plumbing is future
// work).
func mergeTierDefaults(in map[string]TierTurnDefaults) map[string]TierTurnDefaults {
	out := make(map[string]TierTurnDefaults, 3)
	for _, tier := range []struct {
		label    string
		soft     int
		hard     int
	}{
		{tierLabelRoot, defaultTierRootMaxTurns, defaultTierRootMaxTurnsHard},
		{tierLabelMission, defaultTierMissionMaxTurns, defaultTierMissionMaxTurnsHard},
		{tierLabelWorker, defaultTierWorkerMaxTurns, defaultTierWorkerMaxTurnsHard},
	} {
		v := in[tier.label]
		if v.MaxToolTurns <= 0 {
			v.MaxToolTurns = tier.soft
		}
		if v.MaxToolTurnsHard <= 0 {
			v.MaxToolTurnsHard = tier.hard
		}
		if v.StuckDetection.RepeatedHash <= 0 {
			v.StuckDetection.RepeatedHash = defaultStuckRepeatedHash
		}
		if v.StuckDetection.TightDensityCount <= 0 {
			v.StuckDetection.TightDensityCount = defaultStuckTightDensityCount
		}
		if v.StuckDetection.TightDensityWindow <= 0 {
			v.StuckDetection.TightDensityWindow = defaultStuckTightDensityWindow
		}
		out[tier.label] = v
	}
	// Pass through any operator-supplied tier labels outside the
	// canonical set unchanged. Today the parser only accepts
	// root/mission/worker — but a typo (e.g. "missions") would
	// otherwise be silently dropped; preserve it so the runtime
	// caller can warn.
	for k, v := range in {
		if _, ok := out[k]; ok {
			continue
		}
		out[k] = v
	}
	return out
}

// --- HitlView ---

func (s *StaticService) DefaultTimeoutMs() int { return s.hitl.DefaultTimeoutMs }

// --- OnUpdate (shared no-op) ---

// OnUpdate satisfies every View interface's OnUpdate method. The
// static service never fires; the returned cancel is a no-op too.
func (s *StaticService) OnUpdate(fn func()) (cancel func()) {
	_ = fn
	return func() {}
}
