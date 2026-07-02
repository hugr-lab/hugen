// Command a2a is the standalone A2A protocol bridge. It hosts the A2A surface
// (agent card + JSON-RPC/SSE) and drives a hugen instance through its native
// HTTP API (pkg/hugenclient) — out-of-process, so a2a-go stays out of the hugen
// core. Point it at a `hugen serve` endpoint. H8 of design/008-integration.
//
// Config is env (transport/deployment knobs, never YAML):
//
//	HUGEN_API_URL         hugen serve endpoint (required, e.g. http://localhost:10100)
//	HUGEN_API_TOKEN       bearer token the bridge presents to the hugen API
//	                      (the "user" whose sessions these become; empty in dev)
//	HUGEN_A2A_PORT        A2A listener port (default 10000)
//	HUGEN_A2A_BASE_URL    public URL the agent card advertises (tunnel hostname)
//	HUGEN_A2A_API_KEY     gate the A2A endpoint behind X-API-Key
//	HUGEN_A2A_ALLOW_OPEN  =1 to serve the A2A endpoint open (dev only)
//	HUGEN_A2A_AGENT_NAME  override the card name / description
//	HUGEN_A2A_AGENT_DESC
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/hugr-lab/hugen/pkg/a2a"
	"github.com/hugr-lab/hugen/pkg/hugenclient"
)

func main() { os.Exit(run()) }

func run() int {
	logger := newLogger(os.Getenv("HUGEN_LOG_LEVEL"))

	apiURL := strings.TrimSpace(os.Getenv("HUGEN_API_URL"))
	if apiURL == "" {
		logger.Error("a2a: HUGEN_API_URL is required (the hugen serve endpoint)")
		return 2
	}
	client := hugenclient.New(apiURL, hugenclient.WithToken(strings.TrimSpace(os.Getenv("HUGEN_API_TOKEN"))))

	port := envInt("HUGEN_A2A_PORT", 10000)
	baseURL := strings.TrimSpace(os.Getenv("HUGEN_A2A_BASE_URL"))
	if baseURL == "" {
		baseURL = "http://localhost:" + strconv.Itoa(port)
	}

	opts := []a2a.Option{
		a2a.WithLogger(logger),
		a2a.WithListenPort(port),
		a2a.WithBaseURL(baseURL),
		a2a.WithAPIKey(os.Getenv("HUGEN_A2A_API_KEY")),
		a2a.WithAllowOpen(envBool("HUGEN_A2A_ALLOW_OPEN")),
	}
	if name := os.Getenv("HUGEN_A2A_AGENT_NAME"); name != "" {
		opts = append(opts, a2a.WithAgentIdentity(name, os.Getenv("HUGEN_A2A_AGENT_DESC")))
	}
	srv := a2a.New(client, opts...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("a2a: starting bridge", "api", apiURL, "port", port, "base_url", baseURL)
	if err := srv.Run(ctx); err != nil {
		if ctx.Err() != nil {
			logger.Info("a2a: shutdown complete")
			return 0
		}
		logger.Error("a2a: run", "err", err)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	if strings.EqualFold(level, "debug") {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}
