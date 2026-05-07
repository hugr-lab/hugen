package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
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

	mu sync.Mutex
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

// Acquire creates the workspace directory and records the
// per-session ToolManager for Release. Per_session MCP provider
// spawning moved off lifecycle in phase 4.1b-pre stage 5b — the
// pkg/extension/mcp extension owns it now and runs from
// InitState (which fires after Acquire, so workspace paths are
// already on the SessionState by the time the extension reads
// them).
func (r *Resources) Acquire(_ context.Context, s *Session) error {
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

	r.mu.Lock()
	r.children[sessionID] = s.Tools()
	r.mu.Unlock()

	r.deps.Logger.Info("session resources acquired",
		"session", sessionID, "dir", sessDir)
	return nil
}

// Release tears down everything Acquire set up, in reverse order.
// Errors are logged and swallowed — Close must finish even when
// individual subsystems misbehave.
func (r *Resources) Release(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	child := r.children[sessionID]
	delete(r.children, sessionID)
	r.mu.Unlock()

	if child != nil {
		if err := child.Close(); err != nil {
			r.deps.Logger.Warn("session release: tool teardown",
				"session", sessionID, "err", err)
		}
	}
	if r.deps.Workspace != nil {
		if _, err := r.deps.Workspace.Release(sessionID); err != nil {
			r.deps.Logger.Warn("session release: workspace",
				"session", sessionID, "err", err)
		}
	}
	return nil
}

