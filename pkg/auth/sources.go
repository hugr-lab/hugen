package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
)

// AuthSpec is the transport-agnostic input to BuildHugrSource /
// BuildSources: one entry per named auth config in config.yaml.
// Callers translate their own config type (e.g.
// internal/config.AuthConfig) into this shape so pkg/auth stays
// free of project-specific imports.
type AuthSpec struct {
	Name     string
	Type     string // hugr | oidc
	Issuer   string
	ClientID string
	// Deprecated: ignored. The SourceRegistry mounts a single shared
	// /auth/callback on mux; every Source is dispatched by OAuth
	// state prefix. Kept on the struct to keep YAML that still
	// carries callback_path: from breaking the decode.
	CallbackPath string
	BaseURL      string // e.g. http://localhost:10000 — used to build RedirectURL when OIDC path taken
	AccessToken  string
	TokenURL     string
	// LoginPath overrides the default derivation. For Source-based
	// OIDC this is usually "/auth/login/<Name>".
	LoginPath string
	// DiscoverURL is the hugr URL used for type=hugr when no
	// access_token/token_url is set: discovery calls
	// {DiscoverURL}/auth/config to fetch issuer + client_id.
	DiscoverURL string
}

func (s AuthSpec) Primary() string {
	// For backward compatibility with existing YAML configs, treat
	// type=hugr + no explicit primary as a reference to the single
	// hugr Source built in Phase A.
	if strings.EqualFold(s.Type, "hugr") {
		return "hugr"
	}
	return ""
}

// BuildSources registers additional Sources on an existing registry
// — Phase C of the startup sequence. Typically used for MCP
// provider-auth entries from cfg.Auth. Entries with type=hugr become
// aliases on the registry's existing hugr Source instead of standalone
// Sources (reuse the same refreshable token).
//
// The caller is responsible for having already mounted the hugr
// Source (via reg.Add + reg.Mount).
func (s *Service) BuildSources(ctx context.Context, specs []AuthSpec, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	for _, spec := range specs {
		if spec.Name == "" {
			return fmt.Errorf("auth: spec has empty name")
		}
		switch strings.ToLower(spec.Type) {
		case "hugr":
			// type=hugr in provider-auth means "reuse the primary
			// Source" — create an alias instead of a standalone
			// instance. Target must already be registered via
			// AddPrimary.
			target := spec.Primary()
			if target == "" {
				return fmt.Errorf("auth %q: type=hugr but no primary Source registered", spec.Name)
			}
			if spec.Name == target {
				// Trivial self-reference (already registered).
				continue
			}
			if err := s.Alias(spec.Name, target); err != nil {
				return fmt.Errorf("auth %q: alias to %q: %w", spec.Name, target, err)
			}
			logger.Info("auth alias registered", "name", spec.Name, "target", target)

		case "oidc":
			if spec.Issuer == "" || spec.ClientID == "" {
				return fmt.Errorf("auth %q: type=oidc needs issuer + client_id", spec.Name)
			}
			src, err := newOIDCSourceForSpec(ctx, spec, spec.Issuer, spec.ClientID, logger, "oidc")
			if err != nil {
				return err
			}
			if err := s.Add(src); err != nil {
				return err
			}
			if oidc, ok := src.(*oidc.Source); ok {
				s.RegisterPromptLogin(oidc.PromptLogin)
			}

		default:
			return fmt.Errorf("auth %q: unsupported type %q (want hugr|oidc)", spec.Name, spec.Type)
		}
	}
	return nil
}

// newOIDCSourceForSpec builds an OIDCStore for a given AuthSpec
// using the provided issuer + clientID (possibly resolved through
// hugr discovery). Centralises the RedirectURL derivation.
func newOIDCSourceForSpec(ctx context.Context, s AuthSpec, issuer, clientID string, logger *slog.Logger, logType string) (sources.Source, error) {
	redirect := strings.TrimRight(s.BaseURL, "/") + "/auth/callback"
	loginPath := s.LoginPath
	cfg := oidc.Config{
		Name:        s.Name,
		IssuerURL:   issuer,
		ClientID:    clientID,
		RedirectURL: redirect,
		LoginPath:   loginPath,
		Logger:      logger.With("auth", s.Name),
	}
	store, err := oidc.New(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("auth %q: %w", s.Name, err)
	}
	logger.Info("auth source built",
		"name", s.Name, "type", logType, "mode", "oidc", "issuer", issuer)
	return store, nil
}
