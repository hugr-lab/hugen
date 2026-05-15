package config

import (
	"encoding/json"
	"time"
)

// PermissionRule is one entry from operator configuration (Tier
// 1) carrying the same shape as a Hugr-role rule (Tier 2).
// Disabled/Hidden are booleans; Data substitutes into tool args
// via pkg/auth/template.Apply; Filter is AND-merged for
// GraphQL-shaped data tools.
type PermissionRule struct {
	Type     string          `mapstructure:"type"     yaml:"type"`
	Field    string          `mapstructure:"field"    yaml:"field"`
	Disabled bool            `mapstructure:"disabled" yaml:"disabled"`
	Hidden   bool            `mapstructure:"hidden"   yaml:"hidden"`
	Data     json.RawMessage `mapstructure:"data"     yaml:"data,omitempty"`
	Filter   string          `mapstructure:"filter"   yaml:"filter,omitempty"`
}

// ToolProviderSpec describes one provider entry from config:
// either a built-in system provider, a stdio MCP server the
// runtime spawns, or an external HTTP MCP endpoint the runtime
// connects to.
type ToolProviderSpec struct {
	Name      string            `mapstructure:"name"       yaml:"name"`
	Type      string            `mapstructure:"type"       yaml:"type"`
	Transport string            `mapstructure:"transport"  yaml:"transport,omitempty"`
	Lifetime  string            `mapstructure:"lifetime"   yaml:"lifetime,omitempty"`
	Command   string            `mapstructure:"command"    yaml:"command,omitempty"`
	Args      []string          `mapstructure:"args"       yaml:"args,omitempty"`
	Env       map[string]string `mapstructure:"env"        yaml:"env,omitempty"`
	Endpoint  string            `mapstructure:"endpoint"   yaml:"endpoint,omitempty"`
	Headers   map[string]string `mapstructure:"headers"    yaml:"headers,omitempty"`
	Auth      string            `mapstructure:"auth"       yaml:"auth,omitempty"`
}

// AuthSource is the unified config-level shape for an extra auth
// source. Phase 3 carries OIDC, hugr, and api_key types; the
// pkg/auth.Service adapter interprets each.
type AuthSource struct {
	Name         string `mapstructure:"name"          yaml:"name"`
	Type         string `mapstructure:"type"          yaml:"type"`
	Issuer       string `mapstructure:"issuer"        yaml:"issuer,omitempty"`
	ClientID     string `mapstructure:"client_id"     yaml:"client_id,omitempty"`
	CallbackPath string `mapstructure:"callback_path" yaml:"callback_path,omitempty"`
	LoginPath    string `mapstructure:"login_path"    yaml:"login_path,omitempty"`
	AccessToken  string `mapstructure:"access_token"  yaml:"access_token,omitempty"`
	TokenURL     string `mapstructure:"token_url"     yaml:"token_url,omitempty"`
}

// PermissionSettings collects the cross-cutting knobs that
// pkg/auth/perm consumes alongside the rule list.
type PermissionSettings struct {
	RefreshInterval time.Duration `mapstructure:"refresh_interval" yaml:"refresh_interval,omitempty"`
	RemoteEnabled   bool          `mapstructure:"remote_enabled"   yaml:"remote_enabled,omitempty"`
	HardExpiry      time.Duration `mapstructure:"hard_expiry"      yaml:"hard_expiry,omitempty"`
}

// LocalView is the surface pkg/store/local consumers take.
type LocalView interface {
	LocalDB() LocalConfig
	LocalDBEnabled() bool
	OnUpdate(fn func()) (cancel func())
}

// ModelsView is the surface pkg/model / pkg/runtime ModelRouter
// take.
type ModelsView interface {
	ModelsConfig() ModelsConfig
	OnUpdate(fn func()) (cancel func())
}

// EmbeddingView is the surface pkg/store/local pin-embedder takes.
type EmbeddingView interface {
	EmbeddingConfig() EmbeddingConfig
	OnUpdate(fn func()) (cancel func())
}

// AuthView is the surface pkg/auth.Service takes.
type AuthView interface {
	Sources() []AuthSource
	OnUpdate(fn func()) (cancel func())
}

// PermissionsView is the surface pkg/auth/perm consumes. Static
// service publishes a Tier-1 rule list; phase 6+ live reload
// fires OnUpdate when YAML changes.
type PermissionsView interface {
	Rules() []PermissionRule
	RefreshInterval() time.Duration
	RemoteEnabled() bool
	OnUpdate(fn func()) (cancel func())
}

// ToolProvidersView is the surface cmd/hugen / runtime takes when
// it builds the tool catalogue.
type ToolProvidersView interface {
	Providers() []ToolProviderSpec
	OnUpdate(fn func()) (cancel func())
}

// SubagentsView is the operator-config surface for phase-4 sub-
// agent runtime defaults. Per phase-4-spec §3 step 10. Lives next
// to ToolProvidersView / PermissionsView so cmd/hugen can wire the
// session Manager from a single Service handle without per-domain
// fan-out.
//
// The runtime overlays per-skill values from
// skill.HugenMetadata.MaxTurns / MaxTurnsHard / StuckDetection on
// top of these defaults; this view supplies the values when no
// skill is loaded.
type SubagentsView interface {
	// DefaultMaxDepth caps how deep the sub-agent tree can grow
	// from any root. Default 5 if absent. spawn_subagent surfaces
	// ErrDepthExceeded when a request would exceed this ceiling
	// AND no per-spawn override raised it.
	DefaultMaxDepth() int

	// MaxAsyncMissionsPerRoot caps the number of in-flight async
	// missions (spawn_mission with wait="async") per root session.
	// Default 5 if absent. Phase 5.1 § 4.5.
	MaxAsyncMissionsPerRoot() int

	// TierDefaults is the per-tier (root / mission / worker)
	// turn-loop defaults populated by `subagents.tier_defaults` in
	// config.yaml. Phase 5.2 δ (B3 migration). The map is always
	// populated (StaticService auto-materialises the three tiers
	// with the runtime constants on top of any operator override).
	// Implementations always return a fresh copy — callers may
	// mutate the result freely.
	TierDefaults() map[string]TierTurnDefaults

	// MaxParkedChildrenPerRoot caps the number of simultaneously-
	// parked children across a root's subtree. When a new park
	// attempt would exceed the cap, the runtime auto-dismisses the
	// oldest parked child first. Default 3 if absent. Phase 5.2 ε.
	MaxParkedChildrenPerRoot() int

	// ParkedIdleTimeout is the per-parked-child idle deadline.
	// Default 10m if absent. Phase 5.2 ε.
	ParkedIdleTimeout() time.Duration

	OnUpdate(fn func()) (cancel func())
}

// HitlView is the operator-config surface for the phase-5.1 HITL
// primitives. Today it carries one knob — the per-call deadline
// session:inquire uses when the model omits timeout_ms. Future
// HITL knobs (default approval action, max simultaneous inquiries
// per session) extend this view rather than spilling into
// SubagentsView.
type HitlView interface {
	// DefaultTimeoutMs is the per-call session:inquire deadline
	// when the model omits timeout_ms. Default 1 hour if absent.
	// Also acts as the upper-bound clamp — a model that asks for
	// more is silently reduced and a warn is logged. Phase 5.1
	// § 2.7.
	DefaultTimeoutMs() int

	OnUpdate(fn func()) (cancel func())
}

// HitlConfig is the data shape NewStaticService receives via
// StaticInput.Hitl. Absent or zero fields take the runtime
// defaults declared in static.go (DefaultTimeoutMs = 1 hour).
type HitlConfig struct {
	DefaultTimeoutMs int `mapstructure:"default_timeout_ms" yaml:"default_timeout_ms,omitempty"`
}

// StuckPolicy is the operator-config shape mirroring the per-skill
// skill.StuckDetectionPolicy. Kept as a separate type at the
// pkg/config layer so the dependency arrow stays config → skill,
// not the other way: pkg/skill never imports pkg/config. The
// runtime (pkg/session) reconciles the two when picking effective
// values for a turn.
//
// Field semantics match phase-4-spec §4.4 exactly:
//   - RepeatedHash: N consecutive identical tool-call hashes
//     before the rising-edge nudge fires.
//   - TightDensityCount / TightDensityWindow: M same-hash calls
//     within W triggering a density nudge.
//   - Enabled (tri-state): nil = default on, &false = disabled,
//     &true = explicit on. The pointer keeps a missing-key from
//     being conflated with an explicit "enabled: false".
type StuckPolicy struct {
	RepeatedHash       int           `mapstructure:"repeated_hash"        yaml:"repeated_hash,omitempty"`
	TightDensityCount  int           `mapstructure:"tight_density_count"  yaml:"tight_density_count,omitempty"`
	TightDensityWindow time.Duration `mapstructure:"tight_density_window" yaml:"tight_density_window,omitempty"`
	Enabled            *bool         `mapstructure:"enabled"              yaml:"enabled,omitempty"`
}

// IsEnabled resolves the tri-state Enabled to the boolean callers
// actually need. Default true — only an explicit Enabled=&false
// turns the heuristics off.
func (p StuckPolicy) IsEnabled() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// SubagentsConfig is the data shape NewStaticService receives via
// StaticInput. Optional in YAML; absent fields take the runtime
// defaults declared in static.go (MaxDepth=5; per-tier turn-loop
// defaults materialised under TierDefaults).
type SubagentsConfig struct {
	MaxDepth                int `mapstructure:"max_depth"                   yaml:"max_depth,omitempty"`
	MaxAsyncMissionsPerRoot int `mapstructure:"max_async_missions_per_root" yaml:"max_async_missions_per_root,omitempty"`

	// TierDefaults is the per-tier turn-loop defaults block
	// (config.yaml: `subagents.tier_defaults.<tier>`). Phase 5.2 δ
	// (B3 migration): canonical replacement for the deprecated
	// SkillManifest.MaxTurns / MaxTurnsHard / StuckDetection
	// composition. NewStaticService merges the runtime defaults
	// (root 12/24, mission 16/32, worker 40/80) on top of operator
	// values so a partially-specified block still works.
	TierDefaults map[string]TierTurnDefaults `mapstructure:"tier_defaults" yaml:"tier_defaults,omitempty"`

	// Parking governs the parked-subagent lifetime knobs (phase 5.2
	// ε). NewStaticService materialises both fields with runtime
	// defaults when omitted.
	Parking ParkingConfig `mapstructure:"parking" yaml:"parking,omitempty"`
}

// ParkingConfig collects the runtime hygiene caps that bound
// parked-subagent occupancy. Phase 5.2 ε.
//
//   - MaxParkedChildrenPerRoot caps simultaneously-parked children
//     across a root's subtree. When a new park attempt would push
//     the count over the cap, the runtime auto-dismisses the
//     oldest parked child (by ParkedAt) with reason="ceiling_drop"
//     before parking the new one.
//   - ParkedIdleTimeout is the per-child idle clock that starts on
//     entering awaiting_dismissal; on expiry the runtime auto-
//     dismisses with reason="idle_timeout". notify_subagent re-arm
//     clears the timer; the next park starts a fresh clock.
type ParkingConfig struct {
	MaxParkedChildrenPerRoot int           `mapstructure:"max_parked_children_per_root" yaml:"max_parked_children_per_root,omitempty"`
	ParkedIdleTimeout        time.Duration `mapstructure:"parked_idle_timeout"          yaml:"parked_idle_timeout,omitempty"`
}

// TierTurnDefaults is one tier's turn-loop budget. Each field is
// optional; missing values inherit the runtime defaults declared
// in static.go (per-tier). Phase 5.2 δ.
type TierTurnDefaults struct {
	// MaxToolTurns is the per-invocation cap on the
	// model→tool→model loop for sessions at this tier. 0 (absent)
	// falls back to the runtime default for this tier.
	MaxToolTurns int `mapstructure:"max_tool_turns" yaml:"max_tool_turns,omitempty"`

	// MaxToolTurnsHard is the lifetime hard ceiling. 0 falls back
	// to the runtime default for this tier.
	MaxToolTurnsHard int `mapstructure:"max_tool_turns_hard" yaml:"max_tool_turns_hard,omitempty"`

	// StuckDetection mirrors the per-skill StuckPolicy shape.
	// Field-level zero values inherit the runtime defaults.
	StuckDetection StuckPolicy `mapstructure:"stuck_detection" yaml:"stuck_detection,omitempty"`
}
