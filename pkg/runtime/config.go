package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
)

// Config is a fully resolved, env-pure runtime configuration.
//
// All fields are populated by the caller (cmd/hugen projects from
// BootstrapConfig + .env; the scenario harness projects from
// .test.env + per-scenario overrides). Build does not read
// os.Environ, parse .env files, or expand ${VAR} placeholders —
// every value here is final.
type Config struct {
	Logger          *slog.Logger
	Mode            string
	AgentConfigPath string
	StateDir        string
	Workspace       WorkspaceConfig
	Artifacts       ArtifactsConfig
	HTTP            HTTPConfig
	Hugr            HugrConfig

	// AfterAuthHook fires after phaseHTTPAuth has built core.Auth
	// and BEFORE phaseStorage's LoadFromView drains the prompt-login
	// queue. The harness uses this to inject pre-captured OIDC
	// tokens into the hugr source (oidc.Source.SetTokens) so
	// scenarios run against real Hugr without an interactive
	// browser flow. Production callers leave it nil.
	AfterAuthHook func(ctx context.Context, svc *auth.Service) error
}

// WorkspaceConfig — per-session scratch root. Phase 5.4: dirs are
// never deleted on session close. Mission subdirs are reclaimed by
// the orphan sweeper (TTL-based); root dirs are reclaimed by
// phase-6 cron.
type WorkspaceConfig struct {
	Dir string
}

// ArtifactsConfig — the durable, user-facing artifact store (design
// 007). Artifacts are plain files under Dir/<agent>/<root_id>/; the
// folder is the registry (no DB). Empty Dir falls back to
// <StateDir>/artifacts at boot. Size quotas of 0 mean unlimited;
// a publish over a non-zero quota is rejected (no silent eviction).
// IdleTTL of 0 falls back to the 7d default used by the retention
// reaper; artifacts are otherwise deleted only on ROOT-session close.
type ArtifactsConfig struct {
	Dir            string
	MaxTotalSize   int64 // whole store, bytes; 0 = unlimited
	MaxSessionSize int64 // per root_id, bytes; 0 = unlimited
	IdleTTL        time.Duration
}

// HTTPConfig — listener for /api/v1/* and the auth endpoints.
type HTTPConfig struct {
	Port    int
	BaseURI string
}

// HugrConfig — optional remote Hugr platform connection. Required
// when Mode == "remote"; ignored otherwise.
type HugrConfig struct {
	URL         string
	RedirectURI string
	AccessToken string
	TokenURL    string
	Issuer      string
	ClientID    string
	Timeout     time.Duration
}

// Validate enforces minimum invariants Build relies on. Called at
// the top of Build before any IO; returns a wrapped sentinel from
// errors.go.
func (c Config) Validate() error {
	if c.Logger == nil {
		return errInvalid("logger is nil")
	}
	if c.StateDir == "" {
		return errInvalid("state dir is empty")
	}
	switch c.Mode {
	case "local", "remote":
	default:
		return errInvalid("mode must be local or remote, got " + c.Mode)
	}
	if c.Mode == "remote" && c.Hugr.URL == "" {
		return errInvalid("hugr url is required in remote mode")
	}
	return nil
}
