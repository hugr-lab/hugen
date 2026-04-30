package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/tool"
)

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
func buildSessionLifecycle(core *RuntimeCore, ws *sessionWorkspaces) runtime.SessionLifecycle {
	boot := core.Boot
	tools := core.Tools
	log := core.Logger
	cleanup := boot.CleanupOnClose

	return runtime.SessionLifecycle{
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

			provider, err := spawnBashMCP(ctx, boot, sessDir, log)
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

// spawnBashMCP builds the bash-mcp env from BootstrapConfig and
// returns a ready *tool.MCPProvider rooted at sessDir.
func spawnBashMCP(ctx context.Context, boot *BootstrapConfig, sessDir string, log *slog.Logger) (*tool.MCPProvider, error) {
	env := map[string]string{}
	if boot.SharedRoot != "" {
		env["BASH_MCP_SHARED_ROOT"] = boot.SharedRoot
		env["BASH_MCP_SHARED_WRITABLE"] = strconv.FormatBool(boot.SharedWritable)
	}
	// Operator-mounted /readonly/<name>/ entries are out of scope
	// for phase-3 BootstrapConfig — they arrive via PermissionsView
	// Data in a later cut. Until then bash-mcp gets a zero list.

	spec := tool.MCPProviderSpec{
		Name:        "bash-mcp",
		Command:     boot.BashMCPPath,
		Env:         env,
		Cwd:         sessDir,
		Lifetime:    tool.LifetimePerSession,
		PermObject:  "hugen:tool:bash-mcp",
		Description: "session-scoped file + shell tools",
	}
	return tool.NewMCPProvider(ctx, spec, log)
}

