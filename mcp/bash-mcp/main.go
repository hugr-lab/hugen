// Command bash-mcp is the in-tree file-and-shell MCP server,
// spawned by the runtime over stdio. The runtime sets cmd.Dir
// to the session's scratch workspace before spawning; bash-mcp
// itself does no path translation. Convenience file tools and
// shell tools both operate on raw host paths. Sandboxing is
// delegated:
//
//   - In container deployments the Linux kernel + bind mounts
//     constrain what the agent can reach.
//   - In local single-user deployments the OS filesystem ACL
//     and (in phase 5+) HITL approval prompts gate writes.
//
// SHARED_DIR (optional env): a real host path the operator
// designates as the user-visible exchange directory. bash-mcp
// just forwards it to the child shell — the agent is taught
// about it through the _system skill, no path translation.
package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	defaultOutputMaxBytes   = 32 * 1024
	defaultReadMaxBytes     = 1024 * 1024
	defaultDefaultTimeoutMS = 30_000
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("bash-mcp: bootstrap failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := loadConfigFromEnv()
	cwd, _ := os.Getwd()
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	// Workspaces root is the dir that contains every session
	// scratch — by convention the parent of cwd. Operator can
	// override via WORKSPACES_ROOT (e.g. when sessions live two
	// levels deep). Empty disables the cross-session check.
	wsRoot := os.Getenv("WORKSPACES_ROOT")
	if wsRoot == "" && cwd != "" {
		wsRoot = filepath.Dir(cwd)
	}
	if wsRoot != "" {
		if abs, err := filepath.Abs(wsRoot); err == nil {
			wsRoot = abs
		}
	}
	ws := &Workspace{
		SessionDir:     cwd,
		WorkspacesRoot: wsRoot,
	}

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
		"session_dir", cwd,
		"workspaces_root", wsRoot,
		"shared_dir", os.Getenv("SHARED_DIR"),
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
	OutputMaxBytes   int
	ReadMaxBytes     int
	DefaultTimeoutMS int
	MemMB            int
}

func loadConfigFromEnv() bashConfig {
	return bashConfig{
		OutputMaxBytes:   intEnv("BASH_MCP_OUTPUT_MAX_BYTES", defaultOutputMaxBytes),
		ReadMaxBytes:     intEnv("BASH_MCP_READ_MAX_BYTES", defaultReadMaxBytes),
		DefaultTimeoutMS: intEnv("BASH_MCP_DEFAULT_TIMEOUT_MS", defaultDefaultTimeoutMS),
		MemMB:            intEnv("BASH_MCP_MEM_MB", 256),
	}
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
