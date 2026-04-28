package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

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
	BaseURI    string
	Hugr       HugrConfig
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
		Mode:       v.GetString("HUGEN_MODE"),
		LogLevel:   v.GetString("HUGEN_LOG_LEVEL"),
		ConfigPath: v.GetString("HUGEN_CONFIG_FILE"),
		Port:       v.GetInt("HUGEN_PORT"),
		BaseURI:    v.GetString("HUGEN_BASE_URL"),
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
// bootstrap. Phase 1 only consumes Models, LocalDB, Embedding, Auth.
// The legacy A2A / DevUI fields are intentionally absent — those
// modes return in phase 2 (webui) and phase 10 (a2a).
type RuntimeConfig struct {
	Embedding      local.EmbeddingConfig
	localDBEnabled bool
	LocalDB        local.Config
	Models         models.Config
	Auth           []AuthConfig
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
	return nil
}
