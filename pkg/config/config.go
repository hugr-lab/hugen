// Package config composes YAML-shaped sub-configs owned by their
// domain packages into a single Config loaded from .env + config.yaml.
//
// pkg/config does not own domain types — each sub-config (models, a2a,
// devui, store/local) is declared in its owner package and referenced
// here via composition. The only types still owned by pkg/config are
// cross-cutting: HugrConfig (platform connection) and AuthConfig
// (auth provider list).
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/a2a"
	"github.com/hugr-lab/hugen/pkg/devui"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/store/local"
)

// Config is the application configuration: pure composition of
// domain-owned sub-configs.
type Config struct {
	Hugr           HugrConfig
	Identity       local.Identity
	Embedding      local.EmbeddingConfig
	LocalDBEnabled bool
	LocalDB        local.Config

	A2A   a2a.Config
	DevUI devui.Config
	LLM   models.Config

	Auth []AuthConfig
}

// AuthConfig is the YAML-decoded form of an auth entry. It mirrors
// pkg/auth.AuthSpec but carries mapstructure tags for viper — keeps
// pkg/auth YAML-agnostic and lets cmd/hugen/runtime.go translate one
// to the other when wiring the SourceRegistry.
type AuthConfig struct {
	Name         string `mapstructure:"name"`
	Type         string `mapstructure:"type"` // hugr | oidc
	Issuer       string `mapstructure:"issuer"`
	ClientID     string `mapstructure:"client_id"`
	CallbackPath string `mapstructure:"callback_path"`
	LoginPath    string `mapstructure:"login_path"`
	AccessToken  string `mapstructure:"access_token"`
	TokenURL     string `mapstructure:"token_url"`
}

// HugrConfig — platform connection. URL comes from .env:HUGR_URL (not
// YAML); MCPUrl is derived; Auth is a name reference into the Auth
// list used by the hugr LLM client + engine transport.
type HugrConfig struct {
	URL    string
	MCPUrl string
	Auth   string
}

func baseURL(configured string, port int) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// Load is the back-compat single-shot loader: it composes
// LoadBootstrap + LoadLocal and returns the full Config. New
// callers should use LoadBootstrap directly and then dispatch to
// LoadLocal or LoadRemote depending on boot.Remote().
func Load(yamlPath string) (*Config, error) {
	boot, err := LoadBootstrap(".env")
	if err != nil {
		return nil, err
	}
	return LoadLocal(yamlPath, boot)
}

// LoadLocal returns the full Config derived from .env env-defaults
// (carried in boot) plus the YAML file at yamlPath. Passing "" for
// yamlPath skips YAML loading (tests).
func LoadLocal(yamlPath string, boot *BootstrapConfig) (*Config, error) {
	if boot == nil {
		return nil, fmt.Errorf("config: LoadLocal requires BootstrapConfig")
	}
	v := viper.New()
	v.AutomaticEnv()
	v.SetDefault("AGENT_MODEL", "gemma4-26b")
	v.SetDefault("AGENT_MAX_TOKENS", 0)

	cfg := &Config{
		Hugr:     boot.Hugr,
		A2A:      boot.A2A,
		DevUI:    boot.DevUI,
		Identity: boot.Identity,
		LLM: models.Config{
			Model:     v.GetString("AGENT_MODEL"),
			MaxTokens: v.GetInt("AGENT_MAX_TOKENS"),
		},
	}

	if yamlPath != "" {
		if err := applyYAML(cfg, yamlPath); err != nil {
			return nil, fmt.Errorf("config: load yaml %s: %w", yamlPath, err)
		}
	}
	return cfg, nil
}

// expandAuthEnv replaces ${VAR} references with values from the
// process environment in every env-bearing AuthConfig field. Unset
// vars expand to "" — which, for type=hugr, drops us from token mode
// into OIDC discovery (the correct dev-default behaviour).
func expandAuthEnv(list []AuthConfig) {
	for i := range list {
		a := &list[i]
		a.Issuer = os.ExpandEnv(a.Issuer)
		a.ClientID = os.ExpandEnv(a.ClientID)
		a.AccessToken = os.ExpandEnv(a.AccessToken)
		a.TokenURL = os.ExpandEnv(a.TokenURL)
		a.CallbackPath = os.ExpandEnv(a.CallbackPath)
		a.LoginPath = os.ExpandEnv(a.LoginPath)
	}
}

// validateAuth enforces unique auth names + supported types. The
// old "unique callback_path" check went away with the single-/auth/
// callback dispatcher — every Source now shares the same path and
// routing happens on OAuth state prefix at request time.
func validateAuth(list []AuthConfig) error {
	seenNames := map[string]struct{}{}
	for _, a := range list {
		if a.Name == "" {
			return fmt.Errorf("config: auth entry has empty name")
		}
		if _, dup := seenNames[a.Name]; dup {
			return fmt.Errorf("config: duplicate auth name %q", a.Name)
		}
		seenNames[a.Name] = struct{}{}
		switch a.Type {
		case "hugr", "oidc":
			// supported
		default:
			return fmt.Errorf("config: auth %q has unsupported type %q (want hugr|oidc)", a.Name, a.Type)
		}
	}
	return nil
}

// applyYAML unmarshals config.yaml into cfg, overwriting relevant
// sub-configs. The `agent:` key carries Identity (id/short_id/name/type).
func applyYAML(cfg *Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil // YAML is optional
		}
		return err
	}

	y := viper.New()
	y.SetConfigFile(path)
	y.SetConfigType("yaml")
	if err := y.ReadInConfig(); err != nil {
		return err
	}
	return decodeAndFinalize(y, cfg)
}

// decodeAndFinalize runs the full post-source-read pipeline shared by
// YAML and remote paths: unmarshal every section, expand ${ENV_VAR}
// in auth entries, and validate the auth list. Callers only need to
// seed the viper with their source data.
func decodeAndFinalize(v *viper.Viper, cfg *Config) error {
	if err := unmarshalSections(v, cfg); err != nil {
		return err
	}
	expandAuthEnv(cfg.Auth)
	return validateAuth(cfg.Auth)
}

// unmarshalSections is the per-section UnmarshalKey chain that both
// YAML and remote paths feed.
func unmarshalSections(v *viper.Viper, cfg *Config) error {
	if err := v.UnmarshalKey("agent", &cfg.Identity); err != nil {
		return fmt.Errorf("unmarshal agent (identity): %w", err)
	}
	if err := v.UnmarshalKey("a2a", &cfg.A2A); err != nil {
		return fmt.Errorf("unmarshal a2a: %w", err)
	}
	if err := v.UnmarshalKey("devui", &cfg.DevUI); err != nil {
		return fmt.Errorf("unmarshal devui: %w", err)
	}
	cfg.LocalDBEnabled = v.GetBool("local_db_enabled")
	if err := v.UnmarshalKey("local_db", &cfg.LocalDB); err != nil {
		return fmt.Errorf("unmarshal local_db: %w", err)
	}
	if err := v.UnmarshalKey("llm", &cfg.LLM); err != nil {
		return fmt.Errorf("unmarshal llm: %w", err)
	}
	if err := v.UnmarshalKey("embedding", &cfg.Embedding); err != nil {
		return fmt.Errorf("unmarshal embedding: %w", err)
	}
	if a := v.GetString("hugr.auth"); a != "" {
		cfg.Hugr.Auth = a
	}
	if err := v.UnmarshalKey("auth", &cfg.Auth); err != nil {
		return fmt.Errorf("unmarshal auth: %w", err)
	}
	return nil
}
