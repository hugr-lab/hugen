package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/models"

	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/spf13/viper"
)

// upperEnv normalises a viper key (lowercased, dot-separated) to an
// uppercase env var name compatible with os.ExpandEnv lookups.
func upperEnv(k string) string {
	return strings.ToUpper(strings.ReplaceAll(k, ".", "_"))
}

// BootstrapConfig is the .env-driven boot config. It tells the
// process where Hugr lives and which mode to come up in.
type BootstrapConfig struct {
	Mode       string // local | remote
	LogLevel   string
	ConfigPath string
	Port       int
	WebUIPort  int
	BaseURI    string
	// StateDir is the persistent state root (HUGEN_STATE).
	// Used for installed skills (system/community/local), agent
	// keys, and any other across-restart state. Defaults to
	// "${HOME}/.hugen" on local mode.
	StateDir string
	// WorkspaceDir is the per-session scratch root
	// (HUGEN_WORKSPACE_DIR). Each Session.Open creates
	// "<WorkspaceDir>/<session_id>/" and bash-mcp is spawned
	// with cmd.Dir set to it. Defaults to "./.hugen/workspace".
	WorkspaceDir string
	// BashMCPPath is the executable path for the bash-mcp binary.
	// Defaults to "bash-mcp" (resolved via $PATH).
	BashMCPPath string
	// SharedRoot is /shared/ host path mounted into each session's
	// bash-mcp instance. Empty disables /shared/ entirely.
	SharedRoot string
	// SharedWritable controls whether bash.write_file is allowed
	// under /shared/. Defaults to true.
	SharedWritable bool
	// CleanupOnClose removes the session's workspace directory on
	// Session.Close. Defaults to true.
	CleanupOnClose bool
	Hugr           HugrConfig
}

// HugrConfig — platform connection.
type HugrConfig struct {
	URL         string
	RedirectURI string
	AccessToken string // remote mode token
	TokenURL    string // remote mode token URL
	Issuer      string
	ClientID    string
	Timeout     time.Duration
}

func loadBootstrapConfig(envPath string) (*BootstrapConfig, error) {
	v := viper.New()
	if envPath != "" {
		v.SetConfigFile(envPath)
		v.SetConfigType("env")
	}
	v.AutomaticEnv()

	v.SetDefault("HUGR_URL", "http://localhost:15000")
	v.SetDefault("HUGEN_PORT", 10000)
	v.SetDefault("HUGEN_BASH_MCP_PATH", "bash-mcp")
	v.SetDefault("HUGEN_SHARED_WRITABLE", true)
	v.SetDefault("HUGEN_WORKSPACE_CLEANUP_ON_CLOSE", true)
	v.SetDefault("HUGEN_WEBUI_PORT", 10001)
	v.SetDefault("HUGEN_CONFIG_FILE", "config.yaml")
	v.SetDefault("HUGEN_BASE_URL", "http://localhost:10000")

	_ = v.ReadInConfig()

	// Export every key viper read from .env into os.Environ so that
	// os.ExpandEnv calls in downstream config (e.g. pkg/store/local
	// model paths "${LLM_LOCAL_URL}?...") see them.
	for _, k := range v.AllKeys() {
		key := upperEnv(k)
		if os.Getenv(key) != "" {
			continue
		}
		if val := v.GetString(k); val != "" {
			_ = os.Setenv(key, val)
		}
	}

	config := &BootstrapConfig{
		Mode:         v.GetString("HUGEN_MODE"),
		LogLevel:     v.GetString("HUGEN_LOG_LEVEL"),
		ConfigPath:   v.GetString("HUGEN_CONFIG_FILE"),
		Port:         v.GetInt("HUGEN_PORT"),
		WebUIPort:    v.GetInt("HUGEN_WEBUI_PORT"),
		BaseURI:      v.GetString("HUGEN_BASE_URL"),
		StateDir:       v.GetString("HUGEN_STATE"),
		WorkspaceDir:   v.GetString("HUGEN_WORKSPACE_DIR"),
		BashMCPPath:    v.GetString("HUGEN_BASH_MCP_PATH"),
		SharedRoot:     v.GetString("HUGEN_SHARED_ROOT"),
		SharedWritable: v.GetBool("HUGEN_SHARED_WRITABLE"),
		CleanupOnClose: v.GetBool("HUGEN_WORKSPACE_CLEANUP_ON_CLOSE"),
		Hugr: HugrConfig{
			URL:         v.GetString("HUGR_URL"),
			RedirectURI: v.GetString("HUGR_REDIRECT_URI"),
			AccessToken: v.GetString("HUGR_ACCESS_TOKEN"),
			TokenURL:    v.GetString("HUGR_TOKEN_URL"),
			Issuer:      v.GetString("HUGR_ISSUER"),
			ClientID:    v.GetString("HUGR_CLIENT_ID"),
			Timeout:     v.GetDuration("HUGR_TIMEOUT"),
		},
	}
	if config.BaseURI == "" {
		config.BaseURI = fmt.Sprintf("http://localhost:%d", config.Port)
	}
	if config.StateDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			config.StateDir = filepath.Join(home, ".hugen")
		} else {
			config.StateDir = ".hugen"
		}
	}
	if config.WorkspaceDir == "" {
		config.WorkspaceDir = filepath.Join(".hugen", "workspace")
	}
	if config.Hugr.AccessToken != "" && config.Hugr.TokenURL == "" ||
		config.Hugr.AccessToken == "" && config.Hugr.TokenURL != "" {
		return nil, fmt.Errorf("bootstrap: both HUGR_ACCESS_TOKEN and HUGR_TOKEN_URL must be set for remote mode (or both unset for local mode)")
	}
	if config.Hugr.RedirectURI == "" && config.Hugr.URL != "" &&
		config.Hugr.AccessToken == "" && config.Hugr.TokenURL == "" {
		config.Hugr.RedirectURI = fmt.Sprintf("%s/auth/callback", config.BaseURI)
	}
	if config.Hugr.Timeout == 0 && config.Hugr.URL != "" {
		config.Hugr.Timeout = 60 * time.Second
	}
	if config.Mode == "" {
		if config.Hugr.URL != "" && config.Hugr.AccessToken != "" && config.Hugr.TokenURL != "" {
			config.Mode = "remote"
		} else {
			config.Mode = "local"
		}
	}

	return config, nil
}

// IsRemoteMode reports whether the agent is configured as a hub user
// (personal-assistant deployment in design parlance).
func (c *BootstrapConfig) IsRemoteMode() bool { return c.Mode == "remote" }

// IsLocalMode reports whether the agent is autonomous (owns its own
// DB and identity).
func (c *BootstrapConfig) IsLocalMode() bool { return c.Mode == "local" }

// LocalOIDCEnabled reports whether the OIDC browser-flow login UX
// applies (local mode talking to a hugr instance with no static
// access token).
func (c *BootstrapConfig) LocalOIDCEnabled() bool {
	return c.Hugr.URL != "" && c.Hugr.AccessToken == "" && c.Hugr.TokenURL == ""
}

// IdentityMode returns the human-readable label used in startup logs
// to identify the deployment kind: "autonomous-agent" or
// "personal-assistant".
func (c *BootstrapConfig) IdentityMode() string {
	if c.IsRemoteMode() {
		return "personal-assistant"
	}
	return "autonomous-agent"
}

// Info renders the multi-line boot-info block emitted to stderr.
func (c *BootstrapConfig) Info() string {
	w := &strings.Builder{}
	fmt.Fprintf(w, "Starting Hugen on :%d\n", c.Port)
	fmt.Fprintf(w, "Identity mode: %s\n", c.IdentityMode())
	if c.Hugr.URL != "" {
		fmt.Fprintf(w, "Hugr URL: %s\n", c.Hugr.URL)
	}
	if c.LocalOIDCEnabled() {
		fmt.Fprint(w, "Local Hugr oidc browser flow enabled\n")
		if c.Hugr.RedirectURI != "" {
			fmt.Fprintf(w, "Hugr OIDC redirect URI: %s\n", c.Hugr.RedirectURI)
		}
	}
	return w.String()
}

// RuntimeConfig is the YAML-driven config layered on top of the
// bootstrap. Phase 3 adds Permissions, PermSettings, and
// ToolProviders sections; phase 6+ live reload will subscribe via
// pkg/config.Service rather than re-reading this struct.
type RuntimeConfig struct {
	Embedding      local.EmbeddingConfig
	localDBEnabled bool
	LocalDB        local.Config
	Models         models.Config
	Auth           []AuthConfig
	Permissions    []config.PermissionRule    `mapstructure:"permissions"`
	PermSettings   config.PermissionSettings  `mapstructure:"permission_settings"`
	ToolProviders  []config.ToolProviderSpec  `mapstructure:"tool_providers"`
}

// LocalDBEnabled reports whether the embedded engine is on for this
// deployment.
func (c *RuntimeConfig) LocalDBEnabled() bool {
	return c.localDBEnabled
}

// AuthConfig is the YAML-decoded form of an extra auth source entry.
// Phase 1 has no extra sources by default; the field exists so phase-3
// MCP providers can plug in.
type AuthConfig struct {
	Name         string `mapstructure:"name"`
	Type         string `mapstructure:"type"`
	Issuer       string `mapstructure:"issuer"`
	ClientID     string `mapstructure:"client_id"`
	CallbackPath string `mapstructure:"callback_path"`
	LoginPath    string `mapstructure:"login_path"`
	AccessToken  string `mapstructure:"access_token"`
	TokenURL     string `mapstructure:"token_url"`
}

func buildRuntimeConfig(ctx context.Context, boot *BootstrapConfig, src identity.Source) (*RuntimeConfig, error) {
	agent, err := src.Agent(ctx)
	if err != nil {
		return nil, err
	}
	var cfg RuntimeConfig
	v := viper.New()
	if err := v.MergeConfigMap(agent.Config); err != nil {
		return nil, fmt.Errorf("config: merge config map: %w", err)
	}
	if err := unmarshalSections(v, &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal sections: %w", err)
	}
	cfg.localDBEnabled = boot.IsLocalMode()
	if cfg.Models.Model == "" {
		return nil, fmt.Errorf("config: models.model is empty (set in config.yaml)")
	}
	return &cfg, nil
}

func unmarshalSections(v *viper.Viper, cfg *RuntimeConfig) error {
	if err := v.UnmarshalKey("models", &cfg.Models); err != nil {
		return fmt.Errorf("unmarshal models: %w", err)
	}
	if err := v.UnmarshalKey("embedding", &cfg.Embedding); err != nil {
		return fmt.Errorf("unmarshal embedding: %w", err)
	}
	if err := v.UnmarshalKey("local_db", &cfg.LocalDB); err != nil {
		return fmt.Errorf("unmarshal local_db: %w", err)
	}
	if err := v.UnmarshalKey("auth", &cfg.Auth); err != nil {
		return fmt.Errorf("unmarshal auth: %w", err)
	}
	if err := v.UnmarshalKey("permissions", &cfg.Permissions); err != nil {
		return fmt.Errorf("unmarshal permissions: %w", err)
	}
	if err := v.UnmarshalKey("permission_settings", &cfg.PermSettings); err != nil {
		return fmt.Errorf("unmarshal permission_settings: %w", err)
	}
	if err := v.UnmarshalKey("tool_providers", &cfg.ToolProviders); err != nil {
		return fmt.Errorf("unmarshal tool_providers: %w", err)
	}
	return nil
}

// toStaticServiceInput converts the cmd/hugen-internal RuntimeConfig
// aggregate into the pkg/config.StaticInput used by phase-3 consumers.
// Pre-phase-3 wiring (models, local engine) keeps consuming
// RuntimeConfig directly; phase-3 packages take per-domain Views via
// the StaticService.
func (c *RuntimeConfig) toStaticServiceInput() config.StaticInput {
	auth := make([]config.AuthSource, 0, len(c.Auth))
	for _, a := range c.Auth {
		auth = append(auth, config.AuthSource{
			Name:         a.Name,
			Type:         a.Type,
			Issuer:       a.Issuer,
			ClientID:     a.ClientID,
			CallbackPath: a.CallbackPath,
			LoginPath:    a.LoginPath,
			AccessToken:  a.AccessToken,
			TokenURL:     a.TokenURL,
		})
	}
	return config.StaticInput{
		LocalDB:        c.LocalDB,
		LocalDBEnabled: c.localDBEnabled,
		Models:         c.Models,
		Embedding:      c.Embedding,
		Auth:           auth,
		Permissions:    c.Permissions,
		PermSettings:   c.PermSettings,
		ToolProviders:  c.ToolProviders,
	}
}
