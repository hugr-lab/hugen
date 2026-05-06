package session

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	mcpprov "github.com/hugr-lab/hugen/pkg/tool/providers/mcp"
	"github.com/hugr-lab/hugen/pkg/tool/providers/recovery"
)

// Lifecycle is the contract Manager calls on Open and Close. The
// production implementation is *Resources below; tests usually
// pass nil (no per-session resources) or their own struct.
//
// SessionTools returns the per-session ToolManager produced during
// Acquire — a child of the agent-level root with per_session
// providers registered on it. The Session uses this child as its
// dispatch surface so per_session providers shadow the root in a
// natural parent/child walk. Returns nil when no per-session
// scoping happened (no Acquire, lifecycle disabled, etc.) — the
// session keeps its constructor-time tools (typically the root).
type Lifecycle interface {
	Acquire(ctx context.Context, s *Session) error
	Release(ctx context.Context, sessionID string) error
	SessionTools(sessionID string) *tool.ToolManager
}

// ResourceDeps groups every dependency Resources needs.
//
// Providers / Tools / Workspace are required. Skills and SkillStore
// may be nil for deployments that disable autoload.
type ResourceDeps struct {
	Providers  config.ToolProvidersView
	Tools      *tool.ToolManager
	Skills     *skill.SkillManager
	SkillStore skill.SkillStore
	Workspace  *Workspace
	Logger     *slog.Logger

	// SessionType labels the bound session for skill-autoload
	// filtering. Today every session is a "root" session; phase 4
	// makes the type per-session at Open time.
	SessionType string
}

// Resources is the per-session-resources owner. It implements
// Lifecycle: Acquire spawns every per_session MCP provider listed
// in tool_providers, registers them with ToolManager, and autoloads
// skills. Release tears it all down.
//
// Resources is the single source of truth for "what is a session,
// from the runtime's point of view": a workspace dir + a set of
// per-session providers. cmd/hugen only knows how to construct it
// — it does not know about bash-mcp, python-mcp, or any specific
// provider name.
//
// Per-spawn auth credentials (the bootstrap-token pattern used by
// hugr-query and python-mcp) are not the lifecycle's concern —
// those providers are per_agent and mint their own credentials in
// the cmd-level builder. If a future per_session provider needs
// agent-minted credentials, this struct grows a Sources field
// then; until then it stays simple.
type Resources struct {
	deps ResourceDeps

	mu       sync.Mutex
	revokers map[string][]func()
	// children holds the per-session child *tool.ToolManager built
	// during Acquire and consulted by SessionTools. Phase 4.1a
	// stage A step 9 replaced the legacy ToolManager.sessionProviders
	// map with this caller-side map; each child has the agent-level
	// root as its parent so unknown-provider lookups walk up.
	children map[string]*tool.ToolManager
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
		children: make(map[string]*tool.ToolManager),
	}
}

// SessionTools implements Lifecycle.SessionTools — returns the
// child ToolManager built during Acquire for sessionID. Returns
// nil when Acquire has not run (or has been Released) for the id
// — Session keeps its constructor-time tools in that case.
func (r *Resources) SessionTools(sessionID string) *tool.ToolManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.children[sessionID]
}

// Validate inspects every per_session entry in tool_providers and
// returns a fail-fast error for misconfigurations the runtime
// would otherwise hit on the first session.Open.
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
// are removed, the workspace is released.
func (r *Resources) Acquire(ctx context.Context, s *Session) error {
	sessionID := s.id
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

	// Per-session child Manager — phase 4.1a stage A step 9. Each
	// session owns its own child ToolManager whose parent is the
	// agent-level root. Per_session providers register on the
	// child; child.Close() in Release drops them without touching
	// agent-level state.
	child := r.deps.Tools.NewChild()

	var (
		opened   int
		rollback = func() {
			_ = child.Close()
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

		env := make(map[string]string, len(cfg.Env)+2)
		maps.Copy(env, cfg.Env)
		env["SESSION_DIR"] = sessDir
		env["WORKSPACES_ROOT"] = root

		spec := mcpprov.Spec{
			Name:        cfg.Name,
			Command:     cfg.Command,
			Args:        cfg.Args,
			Env:         env,
			Cwd:         sessDir,
			Lifetime:    tool.LifetimePerSession,
			PermObject:  "hugen:tool:" + cfg.Name,
			Description: "session-scoped " + cfg.Name,
			Transport:   mcpprov.TransportStdio,
		}
		inner, err := mcpprov.NewWithSpec(ctx, spec, r.deps.Logger)
		if err != nil {
			rollback()
			return fmt.Errorf("session %s: provider %q spawn: %w", sessionID, cfg.Name, err)
		}
		// Wrap with the lazy retry decorator so per_session MCPs
		// recover transparently from EOF / closed-pipe failures.
		// The decorator delegates to the inner provider's
		// TryReconnect; recovery is driven by the next failed
		// Call/List, not a background goroutine.
		prov := recovery.Wrap(inner, recovery.WithLogger(r.deps.Logger))
		if err := child.AddProvider(prov); err != nil {
			_ = prov.Close()
			rollback()
			return fmt.Errorf("session %s: provider %q register: %w", sessionID, cfg.Name, err)
		}
		opened++
	}

	r.mu.Lock()
	r.children[sessionID] = child
	r.mu.Unlock()

	if r.deps.Skills != nil && r.deps.SkillStore != nil {
		r.autoloadSkills(ctx, sessionID)
	}

	r.deps.Logger.Info("session resources acquired",
		"session", sessionID, "dir", sessDir, "providers", opened)
	return nil
}

// Release tears down everything Acquire set up, in reverse order.
// Errors are logged and swallowed — Close must finish even when
// individual subsystems misbehave.
func (r *Resources) Release(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	child := r.children[sessionID]
	delete(r.children, sessionID)
	revs := r.revokers[sessionID]
	delete(r.revokers, sessionID)
	r.mu.Unlock()

	if child != nil {
		if err := child.Close(); err != nil {
			r.deps.Logger.Warn("session release: tool teardown",
				"session", sessionID, "err", err)
		}
	}
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
