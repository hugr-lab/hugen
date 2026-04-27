package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/models"

	"github.com/hugr-lab/query-engine/client"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adksession "google.golang.org/adk/session"
)

// runtime bundles all long-lived resources built at startup. The
// bare-bones path: hugr client (LLM transport), ADK LLM agent backed
// by *models.HugrModel, in-memory session service.
type runtime struct {
	Agent      adkagent.Agent
	Sessions   adksession.Service
	HugrClient *client.Client
}

func (r *runtime) close(logger *slog.Logger) {
	if r == nil {
		return
	}
	logger.Info("shutting down: closing runtime")
	if r.HugrClient != nil {
		r.HugrClient.CloseSubscriptions()
	}
}

// bootstrap brings up every non-HTTP long-lived component:
// SourceRegistry, hugr client, full config, ADK agent + session
// service. The returned *app owns all of it.
func bootstrap(ctx context.Context, boot *config.BootstrapConfig, logger *slog.Logger) (*app, error) {
	authMux := http.NewServeMux()

	authReg, hugrTransport, err := buildAuthForBootstrap(ctx, boot, authMux, logger)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	hugrClient := client.NewClient(
		boot.Hugr.URL+"/ipc",
		client.WithTransport(hugrTransport),
	)

	cfg, err := loadFullConfig(ctx, boot, hugrClient, logger)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	logger.Info("agent configured",
		"model", cfg.LLM.Model,
		"local_db_enabled", cfg.LocalDBEnabled,
	)

	rt, err := buildRuntime(cfg, logger, hugrClient)
	if err != nil {
		return nil, err
	}

	return &app{
		boot:    boot,
		cfg:     cfg,
		logger:  logger,
		runtime: rt,
		authReg: authReg,
		authMux: authMux,
		prompts: authReg.PromptLogins(),
	}, nil
}

// buildAuthForBootstrap is Phase A+B.1: builds the single hugr Source
// declared by .env, wires it into a fresh SourceRegistry, mounts the
// shared /auth/callback dispatcher, and returns the RoundTripper the
// hugr client should use for outbound calls.
func buildAuthForBootstrap(ctx context.Context, boot *config.BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.SourceRegistry, http.RoundTripper, error) {
	reg := auth.NewSourceRegistry(logger)

	hugrSrc, err := auth.BuildHugrSource(ctx, auth.AuthSpec{
		Name:        boot.HugrAuth.Name,
		Type:        boot.HugrAuth.Type,
		AccessToken: boot.HugrAuth.AccessToken,
		TokenURL:    boot.HugrAuth.TokenURL,
		Issuer:      boot.HugrAuth.Issuer,
		ClientID:    boot.HugrAuth.ClientID,
		BaseURL:     boot.A2A.BaseURL,
		DiscoverURL: boot.Hugr.URL,
	}, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("hugr source: %w", err)
	}
	if err := reg.AddPrimary(hugrSrc); err != nil {
		return nil, nil, err
	}
	if oidc, ok := hugrSrc.(*auth.OIDCStore); ok {
		reg.RegisterPromptLogin(oidc.PromptLogin)
	}
	reg.Mount(mux)

	return reg, auth.Transport(hugrSrc, http.DefaultTransport), nil
}

// loadFullConfig chooses between local YAML and remote hub pull based
// on boot.Remote(). In remote mode it also resolves agent_id via
// whoami before the GraphQL fetch.
func loadFullConfig(ctx context.Context, boot *config.BootstrapConfig, hugrClient *client.Client, logger *slog.Logger) (*config.Config, error) {
	if !boot.Remote() {
		return config.LoadLocal("config.yaml", boot)
	}
	who, err := identity.ResolveFromHugr(ctx, hugrClient)
	if err != nil {
		return nil, fmt.Errorf("remote identity: %w", err)
	}
	boot.Identity.ID = who.UserID
	boot.Identity.Name = who.UserName
	logger.Info("remote identity resolved", "agent_id", who.UserID, "name", who.UserName)

	cfg, err := config.LoadRemote(ctx, hugrClient, boot.Identity.ID, boot)
	if err != nil {
		return nil, fmt.Errorf("remote config: %w", err)
	}
	return cfg, nil
}

// buildRuntime constructs the bare ADK LLM agent: hugr-backed model,
// no toolsets, no callbacks, no instruction provider. Session state
// is held in process memory via session.InMemoryService.
func buildRuntime(cfg *config.Config, logger *slog.Logger, hugrClient *client.Client) (*runtime, error) {
	if cfg.LLM.Model == "" {
		return nil, fmt.Errorf("runtime: cfg.LLM.Model is empty (set AGENT_MODEL or llm.model in config.yaml)")
	}

	opts := []models.Option{
		models.WithLogger(logger),
		models.WithName(cfg.LLM.Model),
		models.WithMaxTokens(cfg.LLM.MaxTokens),
	}
	if cfg.LLM.Temperature > 0 {
		opts = append(opts, models.WithTemperature(cfg.LLM.Temperature))
	}
	llm := models.NewHugr(hugrClient, cfg.LLM.Model, opts...)

	ag, err := llmagent.New(llmagent.Config{
		Name:        agentName(cfg),
		Description: agentDescription(cfg),
		Model:       llm,
	})
	if err != nil {
		return nil, fmt.Errorf("llmagent: %w", err)
	}

	return &runtime{
		Agent:      ag,
		Sessions:   adksession.InMemoryService(),
		HugrClient: hugrClient,
	}, nil
}

func agentName(cfg *config.Config) string {
	if cfg.Identity.Name != "" {
		return cfg.Identity.Name
	}
	return "hugen"
}

func agentDescription(cfg *config.Config) string {
	if cfg.Identity.Type != "" {
		return "Universal Hugr Agent (" + cfg.Identity.Type + ")"
	}
	return "Universal Hugr Agent"
}
