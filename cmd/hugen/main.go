// Package main is the entry point for the hugen runtime.
//
// Phase-1 startup flow:
//
//  1. Load bootstrap config from .env.
//  2. Bring up auth + identity + (local) hugr engine + remote
//     hugr client.
//  3. Build the runtime: ModelRouter → SessionManager → Runtime
//     → console Adapter.
//  4. Dispatch on os.Args[1]:
//     console — runs the native runtime (only mode in phase 1).
//     a2a    — refused (returns in phase 10).
//     webui  — refused (returns in phase 2).
//  5. Block on ctx until SIGINT/SIGTERM, then shut down cleanly.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/adapter/console"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

const (
	exitOK    = 0
	exitUsage = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the testable entry point. It returns the process exit code
// and writes refusal/usage text to errOut.
func run(args []string, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "console":
		return runConsole(ctx)
	case "a2a":
		fmt.Fprintln(errOut, "the a2a mode is not yet available in this build; planned for phase 10")
		return exitUsage
	case "webui":
		fmt.Fprintln(errOut, "the webui mode is not yet available in this build; planned for phase 2")
		return exitUsage
	default:
		fmt.Fprintf(errOut, "unknown subcommand %q\n\n", sub)
		fmt.Fprintln(errOut, "usage: hugen [console]")
		fmt.Fprintln(errOut, "  console  start the console adapter (the only mode in phase 1)")
		return exitUsage
	}
}

func runConsole(ctx context.Context) int {
	boot, err := loadBootstrapConfig(".env")
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}

	logger := newLogger(boot.LogLevel)
	logger.Info("starting hugen", "info", boot.Info())

	// 1. Auth http server (handles OIDC callbacks even though phase 1
	//    is mostly stdin-driven).
	_, mux, err := startHTTPServer(ctx, boot, logger)
	if err != nil {
		log.Printf("auth http: %v", err)
		return 1
	}

	authSvc, err := buildAuthService(ctx, boot, mux, logger)
	if err != nil {
		log.Printf("auth service: %v", err)
		return 1
	}

	// 2. Identity + remote/local engines.
	var remoteQuerier, localQuerier types.Querier
	if boot.Hugr.URL != "" && boot.IsRemoteMode() {
		remoteQuerier = connectRemote(boot, authSvc, logger)
	}
	idSrc := buildIdentity(boot, remoteQuerier)

	// 3. Runtime config (read from local YAML or remote hub).
	cfg, err := buildRuntimeConfig(ctx, boot, idSrc)
	if err != nil {
		log.Printf("runtime config: %v", err)
		return 1
	}

	// 4. Local engine (autonomous mode).
	if cfg.LocalDBEnabled() {
		localQuerier, err = buildLocalEngine(ctx, cfg, idSrc, logger)
		if err != nil {
			log.Printf("local engine: %v", err)
			return 1
		}
	}

	// 5. Models. The Service holds the existing routes; we project
	//    them into a pkg/model.ModelRouter for the runtime.
	modelService := models.New(ctx, localQuerier, remoteQuerier, cfg.Models, models.WithLogger(logger))
	modelMap := models.BuildModelMap(modelService)
	modelDefaults := models.IntentDefaults(modelService)
	router, err := model.NewModelRouter(modelDefaults, modelMap)
	if err != nil {
		log.Printf("model router: %v", err)
		return 1
	}
	logger.Info("model router ready", "default", modelDefaults[model.IntentDefault].String(),
		"cheap", modelDefaults[model.IntentCheap].String())

	// 6. Build runtime components.
	embedderEnabled := cfg.Embedding.Mode != "" && cfg.Embedding.Model != ""
	store := chooseStore(localQuerier, remoteQuerier, embedderEnabled)
	if store == nil {
		log.Printf("runtime store: no querier available (need local engine or remote hub)")
		return 1
	}

	agentInfo, err := idSrc.Agent(ctx)
	if err != nil {
		log.Printf("identity: %v", err)
		return 1
	}
	agent, err := runtime.NewAgent(agentInfo.ID, agentInfo.Name, idSrc)
	if err != nil {
		log.Printf("agent: %v", err)
		return 1
	}

	cmds := runtime.NewCommandRegistry()
	if err := registerBuiltinCommands(cmds, logger); err != nil {
		log.Printf("commands: %v", err)
		return 1
	}

	codec := protocol.NewCodec()
	manager := runtime.NewSessionManager(store, agent, router, cmds, codec, logger)

	// 7. Resume any active session for this agent (Phase 4 semantics).
	resumeID := tryFindResumableSession(ctx, manager, logger)

	// 8. Console adapter.
	consoleAdapter := console.New(
		console.WithLogger(logger),
		consoleResumeOption(resumeID),
		console.WithUser(operatorParticipant()),
	)

	rt := runtime.NewRuntime(manager, []runtime.Adapter{consoleAdapter}, logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // 5s
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

	if err := rt.Start(ctx); err != nil {
		if ctx.Err() != nil {
			logger.Info("shutdown complete")
			return exitOK
		}
		logger.Error("runtime exited", "err", err)
		return 1
	}
	return exitOK
}

// chooseStore prefers the local querier when the embedded engine is
// available; falls back to remote.
func chooseStore(localQ, remoteQ types.Querier, embedderEnabled bool) runtime.RuntimeStore {
	if localQ != nil {
		return runtime.NewRuntimeStoreLocal(localQ, embedderEnabled)
	}
	if remoteQ != nil {
		return runtime.NewRuntimeStoreLocal(remoteQ, embedderEnabled)
	}
	return nil
}

func tryFindResumableSession(ctx context.Context, m *runtime.SessionManager, logger *slog.Logger) string {
	rows, err := m.List(ctx, runtime.StatusActive)
	if err != nil {
		logger.Warn("list active sessions", "err", err)
		return ""
	}
	if len(rows) == 0 {
		return ""
	}
	logger.Info("resumable sessions found", "count", len(rows), "resuming", rows[0].ID)
	return rows[0].ID
}

func consoleResumeOption(id string) console.Option {
	if id == "" {
		return func(*console.Adapter) {}
	}
	return console.WithResumeSession(id)
}

func operatorParticipant() protocol.ParticipantInfo {
	id := os.Getenv("USER")
	if id == "" {
		id = "operator"
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: id}
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	if level == "debug" {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// silence unused import warnings if main grows lean.
var _ identity.Source = (identity.Source)(nil)
var _ *auth.Service
