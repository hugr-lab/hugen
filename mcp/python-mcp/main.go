package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
)

func main() {
	var (
		createTpl string
		out       string
		template  string
	)
	flag.StringVar(&createTpl, "create-template", "", "Path to requirements.txt to build a relocatable venv from (build mode).")
	flag.StringVar(&out, "out", "", "Output dir for --create-template. Default: ${HUGEN_STATE}/python-template/.venv (or ${HOME}/.hugen/...).")
	flag.StringVar(&template, "template", "", "Path to a previously-built relocatable venv (server mode).")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch {
	case createTpl != "" && template != "":
		fail(2, "python-mcp: --create-template and --template are mutually exclusive")
	case createTpl != "":
		if err := runBuildMode(createTpl, out, log); err != nil {
			fail(1, err.Error())
		}
	case template != "":
		if err := runServerMode(template, log); err != nil {
			fail(1, err.Error())
		}
	default:
		fail(2, "python-mcp: one of --create-template or --template is required")
	}
}

func fail(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}

func runBuildMode(reqsPath, outDir string, log *slog.Logger) error {
	if outDir == "" {
		outDir = defaultTemplateDir()
	}
	absReqs, err := filepath.Abs(reqsPath)
	if err != nil {
		return fmt.Errorf("python-mcp: resolve requirements: %w", err)
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("python-mcp: resolve --out: %w", err)
	}
	if _, err := os.Stat(absReqs); err != nil {
		fail(2, fmt.Sprintf("python-mcp: %s: no such file", absReqs))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return BuildTemplate(ctx, absReqs, absOut, log)
}

func runServerMode(template string, log *slog.Logger) error {
	absTpl, err := filepath.Abs(template)
	if err != nil {
		return fmt.Errorf("python-mcp: resolve --template: %w", err)
	}
	workspacesRoot := os.Getenv("WORKSPACES_ROOT")
	if workspacesRoot == "" {
		fail(2, "python-mcp: WORKSPACES_ROOT not set")
	}
	absRoot, err := filepath.Abs(workspacesRoot)
	if err != nil {
		return fmt.Errorf("python-mcp: resolve WORKSPACES_ROOT: %w", err)
	}

	auth, err := loadAuthSource(log)
	if err != nil {
		return err
	}

	deps := &execDeps{
		template:       absTpl,
		workspacesRoot: absRoot,
		auth:           auth,
		log:            log,
	}

	srv := server.NewMCPServer(
		"python-mcp",
		"phase-3.5",
		server.WithToolCapabilities(false),
	)
	registerTools(srv, deps)

	log.Info("python-mcp server starting",
		"template", absTpl,
		"workspaces_root", absRoot,
		"hugr_configured", auth != nil)
	if err := server.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("python-mcp: serve: %w", err)
	}
	return nil
}

// defaultTemplateDir mirrors the operator-facing default documented
// in the contract: ${HUGEN_STATE}/python-template/.venv with a
// ${HOME}/.hugen fallback. Used only when --out is omitted.
func defaultTemplateDir() string {
	if state := os.Getenv("HUGEN_STATE"); state != "" {
		return filepath.Join(state, "python-template", ".venv")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hugen", "python-template", ".venv")
}
