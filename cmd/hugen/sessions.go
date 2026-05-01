package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// autoloadSkills binds every skill that opts into autoload for
// sessionType into the named session. Per-skill failures log a
// warning and continue — one bad bundle must not deny the
// session its working tool surface.
func autoloadSkills(
	ctx context.Context,
	skills *skill.SkillManager,
	store skill.SkillStore,
	sessionID, sessionType string,
	log *slog.Logger,
) {
	all, err := store.List(ctx)
	if err != nil {
		log.Warn("session: list skills for autoload",
			"session", sessionID, "err", err)
		// Partial — keep going with whatever the store returned.
	}
	for _, s := range all {
		if !s.Manifest.AutoloadIn(sessionType) {
			continue
		}
		if err := skills.Load(ctx, sessionID, s.Manifest.Name); err != nil {
			log.Warn("session: autoload skill failed",
				"session", sessionID, "skill", s.Manifest.Name, "err", err)
			continue
		}
		log.Info("session: skill autoloaded",
			"session", sessionID, "skill", s.Manifest.Name)
	}
}

// sessionWorkspaces tracks per-session bookkeeping the lifecycle
// hooks need: the workspace dir to remove on close. Provider
// teardown is delegated to ToolManager.CloseSession; the dir is
// what cmd/hugen owns directly.
type sessionWorkspaces struct {
	mu   sync.Mutex
	dirs map[string]string // sessionID → absolute workspace dir
}

func newSessionWorkspaces() *sessionWorkspaces {
	return &sessionWorkspaces{dirs: make(map[string]string)}
}

func (s *sessionWorkspaces) add(sessionID, dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirs[sessionID] = dir
}

func (s *sessionWorkspaces) take(sessionID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, ok := s.dirs[sessionID]
	if ok {
		delete(s.dirs, sessionID)
	}
	return dir, ok
}

// buildSessionLifecycle wires the per-session bash-mcp lifecycle:
// OnOpen creates the session workspace directory and spawns
// bash-mcp with cmd.Dir set; OnClose tears down the bash-mcp
// process via ToolManager.CloseSession and (when configured)
// removes the workspace dir.
func buildSessionLifecycle(core *RuntimeCore, ws *sessionWorkspaces) session.SessionLifecycle {
	boot := core.Boot
	tools := core.Tools
	skills := core.Skills
	skillStore := core.SkillStore
	providers := core.Config.ToolProviders()
	log := core.Logger
	cleanup := boot.CleanupOnClose

	return session.SessionLifecycle{
		OnOpen: func(ctx context.Context, sessionID string) error {
			absRoot, err := filepath.Abs(boot.WorkspaceDir)
			if err != nil {
				return fmt.Errorf("session %s: resolve workspace dir: %w", sessionID, err)
			}
			sessDir := filepath.Join(absRoot, sessionID)
			if err := os.MkdirAll(sessDir, 0o755); err != nil {
				return fmt.Errorf("session %s: mkdir workspace: %w", sessionID, err)
			}
			ws.add(sessionID, sessDir)

			provider, err := spawnBashMCP(ctx, providers, sessDir, absRoot, log)
			if err != nil {
				if cleanup {
					_ = os.RemoveAll(sessDir)
				}
				ws.take(sessionID)
				return fmt.Errorf("session %s: spawn bash-mcp: %w", sessionID, err)
			}
			if err := tools.AddSessionProvider(sessionID, provider); err != nil {
				_ = provider.Close()
				if cleanup {
					_ = os.RemoveAll(sessDir)
				}
				ws.take(sessionID)
				return fmt.Errorf("session %s: register bash-mcp: %w", sessionID, err)
			}
			// Auto-load every skill whose manifest opts into autoload
			// for this session's type. Phase-3 only opens `root`
			// sessions; the SessionType label is hard-coded here and
			// will become a Session attribute in phase 4. Without
			// any autoload skill loaded, the model boots with an
			// empty allowed-tools filter and reports "no active tools".
			if skills != nil && skillStore != nil {
				autoloadSkills(ctx, skills, skillStore, sessionID, skill.SessionTypeRoot, log)
			}
			log.Info("session workspace ready",
				"session", sessionID, "dir", sessDir)
			return nil
		},
		OnClose: func(ctx context.Context, sessionID string) error {
			if err := tools.CloseSession(ctx, sessionID); err != nil {
				log.Warn("session close: tool teardown",
					"session", sessionID, "err", err)
			}
			dir, ok := ws.take(sessionID)
			if ok && cleanup {
				if err := os.RemoveAll(dir); err != nil {
					log.Warn("session close: rm workspace",
						"session", sessionID, "dir", dir, "err", err)
				}
			}
			return nil
		},
	}
}

// sweepOrphans removes session-workspace directories under
// workspaceDir whose mtime is older than ttl AND whose name is
// not in liveSessions. Catches sessions that crashed mid-flight
// without a clean Close. Returns the count of removed entries.
func sweepOrphans(workspaceDir string, liveSessions map[string]struct{}, ttl time.Duration) (int, error) {
	if ttl <= 0 || workspaceDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, live := liveSessions[e.Name()]; live {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(workspaceDir, e.Name())
		if err := os.RemoveAll(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

// spawnBashMCP looks up the bash-mcp entry in tool_providers and
// spawns it with cmd.Dir = sessDir. Configuration (command, args,
// env: SHARED_DIR) lives in config.yaml — there is no implicit
// default. Returns a clear error when the operator has not
// declared bash-mcp.
//
// SESSION_DIR / WORKSPACES_ROOT are injected here (not in YAML)
// so the runtime is the single source of truth for the cross-
// session boundary: bash-mcp uses them to refuse file-tool paths
// that resolve into a peer session's scratch.
func spawnBashMCP(ctx context.Context, providers config.ToolProvidersView, sessDir, workspacesRoot string, log *slog.Logger) (*tool.MCPProvider, error) {
	cfg, ok := findToolProvider(providers, "bash-mcp")
	if !ok {
		return nil, fmt.Errorf("bash-mcp: not declared in tool_providers (add a per_session stdio entry to config.yaml)")
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("bash-mcp: tool_providers entry has empty command")
	}
	env := make(map[string]string, len(cfg.Env)+2)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["SESSION_DIR"] = sessDir
	env["WORKSPACES_ROOT"] = workspacesRoot
	spec := tool.MCPProviderSpec{
		Name:        cfg.Name,
		Command:     cfg.Command,
		Args:        cfg.Args,
		Env:         env,
		Cwd:         sessDir,
		Lifetime:    tool.LifetimePerSession,
		PermObject:  "hugen:tool:" + cfg.Name,
		Description: "session-scoped file + shell tools",
	}
	return tool.NewMCPProvider(ctx, spec, log)
}

// findToolProvider returns the named entry from a ToolProvidersView.
// Used by per_session lifecycle hooks (bash-mcp today, python-mcp /
// duckdb-mcp in phase 3.5) to read their config.yaml shape without
// duplicating the providers slice.
func findToolProvider(view config.ToolProvidersView, name string) (config.ToolProviderSpec, bool) {
	if view == nil {
		return config.ToolProviderSpec{}, false
	}
	for _, p := range view.Providers() {
		if p.Name == name {
			return p, true
		}
	}
	return config.ToolProviderSpec{}, false
}
