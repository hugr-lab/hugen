// Package mcp is the per_session MCP-spawning extension. Phase
// 4.1b-pre stage 5b moved every per_session provider concern off
// pkg/session/lifecycle.go onto this extension: at session
// InitState the extension reads the agent-level
// [config.ToolProvidersView] for entries with Lifetime=PerSession,
// spawns the matching MCP server inside the session's workspace
// directory, wraps it in the lazy-retry recovery decorator, and
// registers the result on [extension.SessionState.Tools]. On
// CloseSession the extension closes every spawned provider so the
// per_session MCP processes exit cleanly even if the live session
// loop gets cancelled mid-shutdown.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
	mcpprov "github.com/hugr-lab/hugen/pkg/tool/providers/mcp"
	"github.com/hugr-lab/hugen/pkg/tool/providers/recovery"
)

// StateKey is the [extension.SessionState] key the extension
// stores its per-session [*sessionMCP] handle under.
const StateKey = "mcp"

// extensionName is the [extension.Extension.Name] discriminator.
// Doesn't surface as a tool catalogue prefix (this extension does
// not implement [tool.ToolProvider]); kept distinct from any
// individual MCP provider name a user might configure.
const extensionName = "mcp"

// Extension is the agent-level singleton. providers is the
// config.ToolProvidersView wired at boot — read once per
// InitState; logger feeds slog into mcpprov.NewWithSpec and the
// recovery wrapper.
type Extension struct {
	providers config.ToolProvidersView
	logger    *slog.Logger
}

// NewExtension constructs the MCP extension. providers is the
// agent-level [config.ToolProvidersView] (typically
// core.Config.ToolProviders()); logger is the runtime logger.
func NewExtension(providers config.ToolProvidersView, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{providers: providers, logger: logger}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Closer           = (*Extension)(nil)
)

// Name implements [extension.Extension].
func (e *Extension) Name() string { return extensionName }

// sessionMCP is the per-session typed handle stored under
// [StateKey]. Holds the per-session ToolManager pointer the
// extension registered providers on plus the spawned providers
// themselves so CloseSession can drop them.
type sessionMCP struct {
	tm        *tool.ToolManager
	mu        sync.Mutex
	providers []tool.ToolProvider
}

// FromState returns the per-session [*sessionMCP] handle, or nil
// if InitState hasn't run for this session.
func FromState(state extension.SessionState) *sessionMCP {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*sessionMCP)
	return h
}

// InitState implements [extension.StateInitializer]. For every
// per_session entry in the providers view, spawns the MCP server
// inside the calling session's workspace directory, wraps it in
// the lazy-retry recovery decorator, and registers the result on
// state.Tools(). Errors during spawn drop every provider this
// run created so InitState is all-or-nothing per session.
func (e *Extension) InitState(ctx context.Context, state extension.SessionState) error {
	if e.providers == nil {
		state.SetValue(StateKey, &sessionMCP{})
		return nil
	}
	tm := state.Tools()
	if tm == nil {
		return fmt.Errorf("mcp extension: session %s has no per-session ToolManager",
			state.SessionID())
	}
	sessDir, ok := state.WorkspaceDir()
	if !ok {
		// No workspace wired — runtime configured without a
		// per-session scratch dir, which is the test-fixture path.
		// Per_session MCPs need a cwd; bail out cleanly so other
		// extensions still init.
		state.SetValue(StateKey, &sessionMCP{tm: tm})
		return nil
	}
	root, _ := state.WorkspaceRoot()

	h := &sessionMCP{tm: tm}
	state.SetValue(StateKey, h)

	for _, cfg := range e.providers.Providers() {
		if tool.EffectiveLifetime(cfg) != tool.LifetimePerSession {
			continue
		}
		if cfg.Command == "" {
			h.closeAll(state.SessionID(), e.logger)
			return fmt.Errorf("mcp extension: session %s: provider %q has empty command",
				state.SessionID(), cfg.Name)
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
		inner, err := mcpprov.NewWithSpec(ctx, spec, e.logger)
		if err != nil {
			h.closeAll(state.SessionID(), e.logger)
			return fmt.Errorf("mcp extension: session %s: provider %q spawn: %w",
				state.SessionID(), cfg.Name, err)
		}
		// Lazy-retry decorator so per_session MCPs recover
		// transparently from EOF / closed-pipe failures. Recovery
		// is driven by the next failed Call/List, not by a
		// background goroutine.
		prov := recovery.Wrap(inner, recovery.WithLogger(e.logger))
		if err := tm.AddProvider(prov); err != nil {
			_ = prov.Close()
			h.closeAll(state.SessionID(), e.logger)
			return fmt.Errorf("mcp extension: session %s: provider %q register: %w",
				state.SessionID(), cfg.Name, err)
		}
		h.mu.Lock()
		h.providers = append(h.providers, prov)
		h.mu.Unlock()
	}
	return nil
}

// CloseSession implements [extension.Closer]. Closes every
// per_session provider this session spawned. Errors are logged
// but do not abort teardown — close paths must drain regardless.
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	h.closeAll(state.SessionID(), e.logger)
	return nil
}

// closeAll drains every recorded provider, logging errors.
// Called from CloseSession at teardown and from InitState's
// rollback paths. Idempotent — a second call observes an empty
// list.
func (h *sessionMCP) closeAll(sessionID string, logger *slog.Logger) {
	h.mu.Lock()
	provs := h.providers
	h.providers = nil
	h.mu.Unlock()
	for _, p := range provs {
		if err := p.Close(); err != nil && logger != nil {
			logger.Warn("mcp extension: close provider",
				"session", sessionID, "provider", p.Name(), "err", err)
		}
	}
}
