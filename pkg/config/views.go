package config

import (
	"encoding/json"
	"time"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/store/local"
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
	Name     string            `mapstructure:"name"      yaml:"name"`
	Type     string            `mapstructure:"type"      yaml:"type"`
	Lifetime string            `mapstructure:"lifetime"  yaml:"lifetime,omitempty"`
	Command  string            `mapstructure:"command"   yaml:"command,omitempty"`
	Args     []string          `mapstructure:"args"      yaml:"args,omitempty"`
	Env      map[string]string `mapstructure:"env"       yaml:"env,omitempty"`
	URL      string            `mapstructure:"url"       yaml:"url,omitempty"`
	Headers  map[string]string `mapstructure:"headers"   yaml:"headers,omitempty"`
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
	LocalDB() local.Config
	LocalDBEnabled() bool
	OnUpdate(fn func()) (cancel func())
}

// ModelsView is the surface pkg/model / pkg/runtime ModelRouter
// take.
type ModelsView interface {
	ModelsConfig() models.Config
	OnUpdate(fn func()) (cancel func())
}

// EmbeddingView is the surface pkg/store/local pin-embedder takes.
type EmbeddingView interface {
	EmbeddingConfig() local.EmbeddingConfig
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
