package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/runtime"
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
	// StateDir is the persistent state root (HUGEN_STATE).
	// Used for installed skills (system/community/local), agent
	// keys, and any other across-restart state. Defaults to
	// "${HOME}/.hugen" on local mode.
	StateDir string
	// WorkspaceDir is the per-session scratch root
	// (HUGEN_WORKSPACE_DIR). Phase 5.4 layout:
	//   <WorkspaceDir>/<root_id>/                  — chat root
	//   <WorkspaceDir>/<root_id>/<mission_id>/     — mission (workers in
	//                                                the same mission
	//                                                share this dir)
	// bash-mcp is spawned with cmd.Dir set to the session's resolved
	// workspace path. Defaults to "./.hugen/workspace".
	WorkspaceDir string
	// ArtifactsDir is the durable, user-facing artifact store root
	// (HUGEN_ARTIFACTS_DIR) — files under <ArtifactsDir>/<agent>/<root_id>/.
	// Defaults to "<StateDir>/artifacts".
	ArtifactsDir string
	// ArtifactsMaxTotalMB / ArtifactsMaxSessionMB cap the whole store
	// and per-root usage (HUGEN_ARTIFACTS_MAX_TOTAL_MB /
	// HUGEN_ARTIFACTS_MAX_SESSION_MB), MB; 0 = unlimited (v1 default).
	ArtifactsMaxTotalMB   int
	ArtifactsMaxSessionMB int
	// ArtifactsIdleTTL (HUGEN_ARTIFACTS_IDLE_TTL): an open root idle
	// past this is reaped by the retention sweep. Defaults to 7 days.
	ArtifactsIdleTTL time.Duration

	// A2APort (HUGEN_A2A_PORT) selects the A2A adapter's listener mode:
	//   0 (default) → mount on the shared auth/callback listener (Port);
	//   >0          → bind a dedicated listener on this port (recommended
	//                 for tunnel-exposed runs — see design/008
	//                 spec-a2a-adapter.md §6.1 loopback-token caveat).
	// Only consumed by the `hugen a2a` run mode. Transport config is env,
	// never agent YAML.
	A2APort int
	// A2ABaseURL (HUGEN_A2A_BASE_URL) is the public URL the agent card
	// advertises (the tunnel hostname in production). Defaults are derived
	// in runA2A: dedicated → http://localhost:<A2APort>; shared → BaseURI.
	A2ABaseURL string
	// A2AAPIKey (HUGEN_A2A_API_KEY) gates the A2A JSON-RPC endpoint behind a
	// static API key (header X-API-Key). Empty = open endpoint — set it before
	// exposing the endpoint over a tunnel. Transport/auth knob = env, never YAML.
	A2AAPIKey string
	// A2AAllowOpen (HUGEN_A2A_ALLOW_OPEN) explicitly permits serving an
	// unauthenticated A2A endpoint. Without it, `hugen a2a` with no API key
	// FAILS CLOSED (refuses to start). Set =1 for a throwaway local run only.
	A2AAllowOpen bool

	// APIPort (HUGEN_API_PORT) selects the native HTTP API adapter's listener
	// mode: 0 → shared auth/callback listener (Port); >0 → dedicated listener
	// (the norm — one container = one agent = its own port). `hugen serve`.
	APIPort int
	// APIBaseURL (HUGEN_API_BASE_URL) is the public base URL the agent card
	// advertises. Derived in runServe when empty. Transport knob = env.
	APIBaseURL string
	// APIAllowOpen (HUGEN_API_ALLOW_OPEN) permits serving the HTTP API with no
	// token issuer (HUGR_ISSUER) configured. Without it, `hugen serve` FAILS
	// CLOSED — it cannot verify forwarded user tokens (D4). Local dev only.
	APIAllowOpen bool

	Hugr HugrConfig
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
		Mode:                  v.GetString("HUGEN_MODE"),
		LogLevel:              v.GetString("HUGEN_LOG_LEVEL"),
		ConfigPath:            v.GetString("HUGEN_CONFIG_FILE"),
		Port:                  v.GetInt("HUGEN_PORT"),
		BaseURI:               v.GetString("HUGEN_BASE_URL"),
		StateDir:              v.GetString("HUGEN_STATE"),
		WorkspaceDir:          v.GetString("HUGEN_WORKSPACE_DIR"),
		ArtifactsDir:          v.GetString("HUGEN_ARTIFACTS_DIR"),
		ArtifactsMaxTotalMB:   v.GetInt("HUGEN_ARTIFACTS_MAX_TOTAL_MB"),
		ArtifactsMaxSessionMB: v.GetInt("HUGEN_ARTIFACTS_MAX_SESSION_MB"),
		ArtifactsIdleTTL:      v.GetDuration("HUGEN_ARTIFACTS_IDLE_TTL"),
		A2APort:               v.GetInt("HUGEN_A2A_PORT"),
		A2ABaseURL:            v.GetString("HUGEN_A2A_BASE_URL"),
		A2AAPIKey:             v.GetString("HUGEN_A2A_API_KEY"),
		A2AAllowOpen:          v.GetBool("HUGEN_A2A_ALLOW_OPEN"),
		APIPort:               v.GetInt("HUGEN_API_PORT"),
		APIBaseURL:            v.GetString("HUGEN_API_BASE_URL"),
		APIAllowOpen:          v.GetBool("HUGEN_API_ALLOW_OPEN"),
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
	if config.ArtifactsDir == "" {
		config.ArtifactsDir = filepath.Join(config.StateDir, "artifacts")
	}
	if config.ArtifactsIdleTTL == 0 {
		config.ArtifactsIdleTTL = 7 * 24 * time.Hour
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

// projectRuntimeConfig converts BootstrapConfig (env-driven boot
// surface) into the env-pure runtime.Config Build consumes. Every
// runtime.Config field is populated here; runtime.Build does not
// read os.Environ, parse .env files, or expand ${VAR} placeholders
// — projection is the sole entry point for that.
func projectRuntimeConfig(boot *BootstrapConfig, logger *slog.Logger) runtime.Config {
	mode := "local"
	if boot.IsRemoteMode() {
		mode = "remote"
	}
	return runtime.Config{
		Logger:          logger,
		Mode:            mode,
		AgentConfigPath: boot.ConfigPath,
		StateDir:        boot.StateDir,
		Workspace: runtime.WorkspaceConfig{
			Dir: boot.WorkspaceDir,
		},
		Artifacts: runtime.ArtifactsConfig{
			Dir:            boot.ArtifactsDir,
			MaxTotalSize:   int64(boot.ArtifactsMaxTotalMB) << 20,
			MaxSessionSize: int64(boot.ArtifactsMaxSessionMB) << 20,
			IdleTTL:        boot.ArtifactsIdleTTL,
		},
		HTTP: runtime.HTTPConfig{
			Port:    boot.Port,
			BaseURI: boot.BaseURI,
		},
		Hugr: runtime.HugrConfig{
			URL:         boot.Hugr.URL,
			RedirectURI: boot.Hugr.RedirectURI,
			AccessToken: boot.Hugr.AccessToken,
			TokenURL:    boot.Hugr.TokenURL,
			Issuer:      boot.Hugr.Issuer,
			ClientID:    boot.Hugr.ClientID,
			Timeout:     boot.Hugr.Timeout,
		},
	}
}
