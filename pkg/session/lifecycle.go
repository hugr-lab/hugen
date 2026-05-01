package session

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth/spawn"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Lifecycle is the contract Manager calls on Open and Close. The
// production implementation is *Resources below; tests usually
// pass nil (no per-session resources) or their own struct.
type Lifecycle interface {
	Acquire(ctx context.Context, sessionID string) error
	Release(ctx context.Context, sessionID string) error
}

// ResourceDeps groups every dependency Resources needs.
//
// Providers / Tools / Workspace are required. Skills and SkillStore
// may be nil for deployments that disable autoload. AuthSources
// may be nil if no provider in tool_providers declares an `auth:`
// field — Validate enforces that constraint at boot.
type ResourceDeps struct {
	Providers   config.ToolProvidersView
	Tools       *tool.ToolManager
	Skills      *skill.SkillManager
	SkillStore  skill.SkillStore
	Workspace   *Workspace
	AuthSources *spawn.Sources
	Logger      *slog.Logger

	// SessionType labels the bound session for skill-autoload
	// filtering. Today every session is a "root" session; phase 4
	// makes the type per-session at Open time.
	SessionType string
}

// Resources is the per-session-resources owner. It implements
// Lifecycle: Acquire spawns every per_session MCP provider listed
// in tool_providers, composes their auth env via the spawn.Sources
// registry, registers them with ToolManager, and autoloads skills.
// Release tears it all down.
//
// Resources is the single source of truth for "what is a session,
// from the runtime's point of view": a workspace dir + a set of
// per-session providers + a set of revoke callbacks. cmd/hugen
// only knows how to construct it — it does not know about bash-mcp,
// python-mcp, or any specific provider name.
type Resources struct {
	deps ResourceDeps

	mu       sync.Mutex
	revokers map[string][]func()
}

// NewResources constructs a Resources owner from its deps. Every
// required dep is checked at first use, not in the constructor —
// nil Tools / Workspace produce a clear error from Acquire instead
// of a panic.
func NewResources(deps ResourceDeps) *Resources {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.SessionType == "" {
		deps.SessionType = skill.SessionTypeRoot
	}
	return &Resources{
		deps:     deps,
		revokers: make(map[string][]func()),
	}
}

// Validate inspects every per_session entry in tool_providers and
// returns a fail-fast error for misconfigurations the runtime
// would otherwise hit on the first session.Open: empty command,
// or `auth: <name>` referring to a source that was not registered.
//
// Called once during boot from cmd/hugen/runtime.go.
func (r *Resources) Validate() error {
	if r == nil || r.deps.Providers == nil {
		return nil
	}
	var errs []string
	for _, cfg := range r.deps.Providers.Providers() {
		if tool.EffectiveLifetime(cfg) != tool.LifetimePerSession {
			continue
		}
		if cfg.Command == "" {
			errs = append(errs, fmt.Sprintf("provider %q: empty command", cfg.Name))
		}
		name := strings.ToLower(strings.TrimSpace(cfg.Auth))
		if name == "" {
			continue
		}
		if r.deps.AuthSources == nil {
			errs = append(errs, fmt.Sprintf(
				"provider %q: auth: %q but no auth-source registry configured",
				cfg.Name, name))
			continue
		}
		if _, ok := r.deps.AuthSources.Get(name); !ok {
			errs = append(errs, fmt.Sprintf(
				"provider %q: auth: %q but no such source registered (have: %v)",
				cfg.Name, name, r.deps.AuthSources.Names()))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("session lifecycle validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// Acquire creates the workspace directory, spawns every
// per_session provider, registers each with ToolManager, autoloads
// skills, and stores the per-session revokers for Release.
//
// On any failure, the partial state is unwound: spawned providers
// are removed, auth revokers run, the workspace is released.
func (r *Resources) Acquire(ctx context.Context, sessionID string) error {
	if r.deps.Workspace == nil {
		return fmt.Errorf("session %s: workspace not configured", sessionID)
	}
	if r.deps.Tools == nil {
		return fmt.Errorf("session %s: tool manager not configured", sessionID)
	}

	sessDir, err := r.deps.Workspace.Acquire(sessionID)
	if err != nil {
		return fmt.Errorf("session %s: %w", sessionID, err)
	}
	root, _ := r.deps.Workspace.Root()

	// Tracks rollback work that must run on partial-failure paths.
	var (
		revokes  []func()
		opened   []string
		rollback = func() {
			for _, name := range opened {
				_ = r.deps.Tools.RemoveSessionProvider(ctx, sessionID, name)
			}
			for _, rv := range revokes {
				rv()
			}
			_, _ = r.deps.Workspace.Release(sessionID)
		}
	)

	for _, cfg := range r.deps.Providers.Providers() {
		if tool.EffectiveLifetime(cfg) != tool.LifetimePerSession {
			continue
		}
		if cfg.Command == "" {
			rollback()
			return fmt.Errorf("session %s: provider %q has empty command", sessionID, cfg.Name)
		}

		authEnv, revoke, err := r.prepareAuth(ctx, cfg, sessionID)
		if err != nil {
			rollback()
			return fmt.Errorf("session %s: provider %q auth: %w", sessionID, cfg.Name, err)
		}
		if revoke != nil {
			revokes = append(revokes, revoke)
		}

		env := make(map[string]string, len(cfg.Env)+len(authEnv)+2)
		maps.Copy(env, cfg.Env)
		maps.Copy(env, authEnv)
		env["SESSION_DIR"] = sessDir
		env["WORKSPACES_ROOT"] = root

		spec := tool.MCPProviderSpec{
			Name:        cfg.Name,
			Command:     cfg.Command,
			Args:        cfg.Args,
			Env:         env,
			Cwd:         sessDir,
			Lifetime:    tool.LifetimePerSession,
			PermObject:  "hugen:tool:" + cfg.Name,
			Description: "session-scoped " + cfg.Name,
			Transport:   tool.TransportStdio,
		}
		prov, err := tool.NewMCPProvider(ctx, spec, r.deps.Logger)
		if err != nil {
			rollback()
			return fmt.Errorf("session %s: provider %q spawn: %w", sessionID, cfg.Name, err)
		}
		if err := r.deps.Tools.AddSessionProvider(sessionID, prov); err != nil {
			_ = prov.Close()
			rollback()
			return fmt.Errorf("session %s: provider %q register: %w", sessionID, cfg.Name, err)
		}
		opened = append(opened, cfg.Name)
	}

	r.mu.Lock()
	r.revokers[sessionID] = revokes
	r.mu.Unlock()

	if r.deps.Skills != nil && r.deps.SkillStore != nil {
		r.autoloadSkills(ctx, sessionID)
	}

	r.deps.Logger.Info("session resources acquired",
		"session", sessionID, "dir", sessDir, "providers", len(opened))
	return nil
}

// Release tears down everything Acquire set up, in reverse order.
// Errors are logged and swallowed — Close must finish even when
// individual subsystems misbehave.
func (r *Resources) Release(ctx context.Context, sessionID string) error {
	if r.deps.Tools != nil {
		if err := r.deps.Tools.CloseSession(ctx, sessionID); err != nil {
			r.deps.Logger.Warn("session release: tool teardown",
				"session", sessionID, "err", err)
		}
	}
	r.mu.Lock()
	revs := r.revokers[sessionID]
	delete(r.revokers, sessionID)
	r.mu.Unlock()
	for _, rv := range revs {
		rv()
	}
	if r.deps.Workspace != nil {
		if _, err := r.deps.Workspace.Release(sessionID); err != nil {
			r.deps.Logger.Warn("session release: workspace",
				"session", sessionID, "err", err)
		}
	}
	return nil
}

// prepareAuth resolves cfg.Auth (case-insensitive, trimmed) to a
// registered spawn.Source and asks it for env + revoke. An empty
// `auth:` returns (nil, nil, nil) — the provider runs without
// agent-minted credentials.
func (r *Resources) prepareAuth(ctx context.Context, cfg config.ToolProviderSpec, sessionID string) (map[string]string, func(), error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Auth))
	if name == "" {
		return nil, nil, nil
	}
	if r.deps.AuthSources == nil {
		return nil, nil, fmt.Errorf("auth: %q requested but no auth-source registry configured", name)
	}
	src, ok := r.deps.AuthSources.Get(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown auth source %q (registered: %v)", name, r.deps.AuthSources.Names())
	}
	return src.Env(ctx, sessionID)
}

// autoloadSkills binds every skill that opts into autoload for
// the configured SessionType. Per-skill failures log a warning
// and continue — one bad bundle must not deny the session its
// working tool surface.
func (r *Resources) autoloadSkills(ctx context.Context, sessionID string) {
	all, err := r.deps.SkillStore.List(ctx)
	if err != nil {
		r.deps.Logger.Warn("session: list skills for autoload",
			"session", sessionID, "err", err)
	}
	for _, s := range all {
		if !s.Manifest.AutoloadIn(r.deps.SessionType) {
			continue
		}
		if err := r.deps.Skills.Load(ctx, sessionID, s.Manifest.Name); err != nil {
			r.deps.Logger.Warn("session: autoload skill failed",
				"session", sessionID, "skill", s.Manifest.Name, "err", err)
			continue
		}
		r.deps.Logger.Info("session: skill autoloaded",
			"session", sessionID, "skill", s.Manifest.Name)
	}
}
