package session

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
	mcpprov "github.com/hugr-lab/hugen/pkg/tool/providers/mcp"
	"github.com/hugr-lab/hugen/pkg/tool/providers/recovery"
)

// Lifecycle is the contract Manager calls on Open and Close. The
// production implementation is *Resources below; tests usually
// pass nil (no per-session resources) or their own struct.
//
// Acquire receives the freshly-constructed Session. By the time
// Acquire runs, s.Tools() is already a per-session child manager
// (NewSession derived it off the root). Acquire's job is to add
// per_session MCP providers to s.Tools() and any other per-session
// state (workspace dir, autoloaded skills, …).
type Lifecycle interface {
	Acquire(ctx context.Context, s *Session) error
	Release(ctx context.Context, sessionID string) error
}

// ResourceDeps groups every dependency Resources needs.
//
// Providers and Workspace are required. Skill autoload moved off
// Resources in phase 4.1b-pre stage 5 — the skill extension now
// owns it (triggered from its own InitState), so Resources stops
// touching SkillManager / SkillStore entirely.
type ResourceDeps struct {
	Providers config.ToolProvidersView
	Workspace *Workspace
	Logger    *slog.Logger
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
	// children records the per-session child ToolManager (= s.Tools())
	// at Acquire time so Release can close it when the session goes
	// away. The session itself owns the child via NewSession; this
	// map is just the lifecycle-side handle for teardown.
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
	return &Resources{
		deps:     deps,
		revokers: make(map[string][]func()),
		children: make(map[string]*tool.ToolManager),
	}
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
	if s.Tools() == nil {
		return fmt.Errorf("session %s: per-session tool manager not configured", sessionID)
	}

	sessDir, err := r.deps.Workspace.Acquire(sessionID)
	if err != nil {
		return fmt.Errorf("session %s: %w", sessionID, err)
	}
	root, _ := r.deps.Workspace.Root()

	// The session's per-session child ToolManager — derived from
	// the agent-level root inside NewSession. Per_session providers
	// register on this child; child.Close() in Release drops them
	// without touching agent-level state.
	child := s.Tools()

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

