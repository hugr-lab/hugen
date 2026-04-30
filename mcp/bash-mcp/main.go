// Command bash-mcp is the in-tree file-and-shell MCP server,
// spawned by the runtime over stdio (per-agent lifetime). It
// exposes the bash.run, bash.shell, bash.read_file,
// bash.write_file, bash.list_dir, and bash.sed tools against a
// three-roots workspace: /workspace/<sid>/ (per-session,
// ephemeral), /shared/<aid>/ (agent-wide), and /readonly/<name>/
// (deployment mounts, read-only).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	defaultOutputMaxBytes   = 32 * 1024
	defaultReadMaxBytes     = 1024 * 1024
	defaultDefaultTimeoutMS = 30_000
	defaultOrphanTTLMS      = 60 * 60 * 1000
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("bash-mcp: bootstrap failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}
	ws := &Workspace{
		WorkspaceRoot: cfg.WorkspaceRoot,
		SharedRoot:    cfg.SharedRoot,
		ReadonlyMnt:   cfg.ReadonlyMounts,
		AgentID:       cfg.AgentID,
		SessionID:     cfg.SessionID,
		OrphanTTL:     time.Duration(cfg.OrphanTTLMS) * time.Millisecond,
	}
	if err := ws.Validate(); err != nil {
		return err
	}
	if err := ws.EnsureSessionDirs(); err != nil {
		return err
	}
	go func() {
		if removed, err := ws.SweepOrphans(); err != nil {
			log.Warn("bash-mcp: orphan sweep failed", "err", err)
		} else if removed > 0 {
			log.Info("bash-mcp: orphan sweep", "removed", removed)
		}
	}()

	tools := &Tools{
		WS: ws,
		Limits: Limits{
			OutputMaxBytes:   cfg.OutputMaxBytes,
			ReadMaxBytes:     cfg.ReadMaxBytes,
			DefaultTimeoutMS: cfg.DefaultTimeoutMS,
			MemMB:            cfg.MemMB,
		},
	}
	srv := server.NewMCPServer(
		"bash-mcp",
		"phase-3",
		server.WithToolCapabilities(true),
	)
	tools.Register(&serverAdapter{s: srv})

	log.Info("bash-mcp: starting stdio server",
		"session_id", cfg.SessionID,
		"agent_id", cfg.AgentID,
		"workspace_root", cfg.WorkspaceRoot,
		"shared_root", cfg.SharedRoot,
		"readonly_mounts", len(cfg.ReadonlyMounts),
	)
	return server.ServeStdio(srv)
}

// serverAdapter wraps *server.MCPServer.AddTool so the bare-func
// signature in mcpToolRegistrar (tools.go) lines up with the
// library's named ToolHandlerFunc type.
type serverAdapter struct{ s *server.MCPServer }

func (a *serverAdapter) AddTool(tool mcp.Tool, handler func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) {
	a.s.AddTool(tool, server.ToolHandlerFunc(handler))
}

type bashConfig struct {
	WorkspaceRoot    string
	SharedRoot       string
	ReadonlyMounts   []ReadonlyMnt
	AgentID          string
	SessionID        string
	OutputMaxBytes   int
	ReadMaxBytes     int
	DefaultTimeoutMS int
	OrphanTTLMS      int
	MemMB            int
}

func loadConfigFromEnv() (bashConfig, error) {
	cfg := bashConfig{
		WorkspaceRoot:    envOrDefault("BASH_MCP_WORKSPACE_ROOT", "/workspace"),
		SharedRoot:       envOrDefault("BASH_MCP_SHARED_ROOT", "/shared"),
		AgentID:          os.Getenv("BASH_MCP_AGENT_ID"),
		SessionID:        os.Getenv("BASH_MCP_SESSION_ID"),
		OutputMaxBytes:   intEnv("BASH_MCP_OUTPUT_MAX_BYTES", defaultOutputMaxBytes),
		ReadMaxBytes:     intEnv("BASH_MCP_READ_MAX_BYTES", defaultReadMaxBytes),
		DefaultTimeoutMS: intEnv("BASH_MCP_DEFAULT_TIMEOUT_MS", defaultDefaultTimeoutMS),
		OrphanTTLMS:      intEnv("BASH_MCP_ORPHAN_TTL_MS", defaultOrphanTTLMS),
		MemMB:            intEnv("BASH_MCP_MEM_MB", 256),
	}
	if cfg.AgentID == "" {
		return cfg, fmt.Errorf("bash-mcp: BASH_MCP_AGENT_ID not set")
	}
	if cfg.SessionID == "" {
		return cfg, fmt.Errorf("bash-mcp: BASH_MCP_SESSION_ID not set")
	}
	if raw := os.Getenv("BASH_MCP_READONLY_MOUNTS"); raw != "" {
		var entries []ReadonlyMnt
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return cfg, fmt.Errorf("bash-mcp: BASH_MCP_READONLY_MOUNTS invalid JSON: %w", err)
		}
		cfg.ReadonlyMounts = entries
	}
	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
