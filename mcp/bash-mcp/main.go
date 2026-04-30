// Command bash-mcp is the in-tree file-and-shell MCP server,
// spawned by the runtime over stdio. It is stateless: the
// runtime sets cmd.Dir to the session's writable workspace
// directory before spawning, and bash-mcp just resolves logical
// paths against (cwd, /shared, /readonly/<name>). The session
// id never crosses the bash-mcp boundary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}
	ws := &Workspace{
		SharedRoot:     cfg.SharedRoot,
		SharedWritable: cfg.SharedWritable,
		ReadonlyMnt:    cfg.ReadonlyMounts,
	}
	if err := ws.Validate(); err != nil {
		return err
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

	cwd, _ := os.Getwd()
	log.Info("bash-mcp: starting stdio server",
		"cwd", cwd,
		"shared_root", cfg.SharedRoot,
		"shared_writable", cfg.SharedWritable,
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
	SharedRoot       string
	SharedWritable   bool
	ReadonlyMounts   []ReadonlyMnt
	OutputMaxBytes   int
	ReadMaxBytes     int
	DefaultTimeoutMS int
	MemMB            int
}

func loadConfigFromEnv() (bashConfig, error) {
	cfg := bashConfig{
		SharedRoot:       os.Getenv("BASH_MCP_SHARED_ROOT"),
		SharedWritable:   boolEnv("BASH_MCP_SHARED_WRITABLE", true),
		OutputMaxBytes:   intEnv("BASH_MCP_OUTPUT_MAX_BYTES", defaultOutputMaxBytes),
		ReadMaxBytes:     intEnv("BASH_MCP_READ_MAX_BYTES", defaultReadMaxBytes),
		DefaultTimeoutMS: intEnv("BASH_MCP_DEFAULT_TIMEOUT_MS", defaultDefaultTimeoutMS),
		MemMB:            intEnv("BASH_MCP_MEM_MB", 256),
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

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func boolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes":
		return true
	case "0", "false", "FALSE", "False", "no":
		return false
	}
	return def
}
