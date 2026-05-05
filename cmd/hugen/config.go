package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	v.SetDefault("HUGEN_WORKSPACE_CLEANUP_ON_CLOSE", true)
	v.SetDefault("HUGEN_WEBUI_PORT", 10001)
	v.SetDefault("HUGEN_CONFIG_FILE", "config.yaml")
	v.SetDefault("HUGEN_BASE_URL", "http://localhost:10000")

	_ = v.ReadInConfig()

	// Force PWD to hugen's actual working directory before
	// expanding .env values. The shell-set PWD survives most
	// launchers (terminal, make, go run), but `dlv exec` and some
	// IDE debug paths inherit a stale or absent PWD — leading to
	// `${PWD}/data` resolving against the wrong root or vanishing.
	// os.Getwd is what every other path inside the process uses, so
	// keying off it gives consistent results across launchers.
	if cwd, err := os.Getwd(); err == nil {
		_ = os.Setenv("PWD", cwd)
	}

	// Export every key viper read from .env into os.Environ so that
	// os.ExpandEnv calls in downstream config (e.g. pkg/store/local
	// model paths "${LLM_LOCAL_URL}?...") see them. Values are
	// expanded once before export so .env can reference shell vars
	// (e.g. HUGEN_SHARED_ROOT=${PWD}/data) — pkg/config/loader
	// runs only one ExpandEnv pass on its string fields, so without
	// this pre-pass nested `${VAR}` placeholders would survive into
	// the final config and reach MCP subprocesses verbatim.
	for _, k := range v.AllKeys() {
		key := upperEnv(k)
		if os.Getenv(key) != "" {
			continue
		}
		val := v.GetString(k)
		if val == "" {
			continue
		}
		_ = os.Setenv(key, os.ExpandEnv(val))
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
