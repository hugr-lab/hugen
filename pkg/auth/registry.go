package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// BuildHugrSource builds the single Source needed for the hugr
// connection (Phase A of the startup sequence). Chooses between
// RemoteStore (when AccessToken + TokenURL are set) and OIDCStore
// with discovery through {DiscoverURL}/auth/config.
//
// The returned Source is NOT yet registered in any SourceRegistry
// — callers pass it to reg.Add and reg.Mount.
func BuildHugrSource(ctx context.Context, spec AuthSpec, logger *slog.Logger) (Source, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("auth: hugr source has empty name")
	}
	if spec.AccessToken != "" && spec.TokenURL != "" {
		logger.Info("auth source built",
			"name", spec.Name, "type", "hugr", "mode", "token", "token_url", spec.TokenURL)
		return NewRemoteStore(spec.Name, spec.AccessToken, spec.TokenURL), nil
	}

	issuer := spec.Issuer
	clientID := spec.ClientID
	if (issuer == "" || clientID == "") && spec.DiscoverURL != "" {
		disc, err := DiscoverOIDCFromHugr(ctx, spec.DiscoverURL)
		if err != nil {
			return nil, fmt.Errorf("auth %q: discover from %s: %w", spec.Name, spec.DiscoverURL, err)
		}
		if disc == nil {
			return nil, fmt.Errorf("auth %q: discover from %s returned empty config", spec.Name, spec.DiscoverURL)
		}
		if issuer == "" {
			issuer = disc.Issuer
		}
		if clientID == "" {
			clientID = disc.ClientID
		}
	}
	if issuer == "" || clientID == "" {
		return nil, fmt.Errorf("auth %q: no token_url/access_token and discovery did not yield issuer+client_id", spec.Name)
	}
	return newOIDCSourceForSpec(ctx, spec, issuer, clientID, logger, "hugr")
}

// BuildSources registers additional Sources on an existing registry
// — Phase C of the startup sequence. Typically used for MCP
// provider-auth entries from cfg.Auth. Entries with type=hugr become
// aliases on the registry's existing hugr Source instead of standalone
// Sources (reuse the same refreshable token).
//
// The caller is responsible for having already mounted the hugr
// Source (via reg.Add + reg.Mount).
func BuildSources(ctx context.Context, specs []AuthSpec, reg *SourceRegistry, logger *slog.Logger) error {
	if reg == nil {
		return fmt.Errorf("auth: SourceRegistry is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	for _, s := range specs {
		if s.Name == "" {
			return fmt.Errorf("auth: spec has empty name")
		}
		switch strings.ToLower(s.Type) {
		case "hugr":
			// type=hugr in provider-auth means "reuse the primary
			// Source" — create an alias instead of a standalone
			// instance. Target must already be registered via
			// AddPrimary.
			target := reg.Primary()
			if target == "" {
				return fmt.Errorf("auth %q: type=hugr but no primary Source registered", s.Name)
			}
			if s.Name == target {
				// Trivial self-reference (already registered).
				continue
			}
			if err := reg.Alias(s.Name, target); err != nil {
				return fmt.Errorf("auth %q: alias to %q: %w", s.Name, target, err)
			}
			logger.Info("auth alias registered", "name", s.Name, "target", target)

		case "oidc":
			if s.Issuer == "" || s.ClientID == "" {
				return fmt.Errorf("auth %q: type=oidc needs issuer + client_id", s.Name)
			}
			src, err := newOIDCSourceForSpec(ctx, s, s.Issuer, s.ClientID, logger, "oidc")
			if err != nil {
				return err
			}
			if err := reg.Add(src); err != nil {
				return err
			}
			if oidc, ok := src.(*OIDCStore); ok {
				reg.RegisterPromptLogin(oidc.PromptLogin)
			}

		default:
			return fmt.Errorf("auth %q: unsupported type %q (want hugr|oidc)", s.Name, s.Type)
		}
	}
	return nil
}

// newOIDCSourceForSpec builds an OIDCStore for a given AuthSpec
// using the provided issuer + clientID (possibly resolved through
// hugr discovery). Centralises the RedirectURL derivation.
func newOIDCSourceForSpec(ctx context.Context, s AuthSpec, issuer, clientID string, logger *slog.Logger, logType string) (Source, error) {
	redirect := strings.TrimRight(s.BaseURL, "/") + "/auth/callback"
	loginPath := s.LoginPath
	cfg := OIDCConfig{
		Name:        s.Name,
		IssuerURL:   issuer,
		ClientID:    clientID,
		RedirectURL: redirect,
		LoginPath:   loginPath,
		Logger:      logger.With("auth", s.Name),
	}
	store, err := NewOIDCStore(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("auth %q: %w", s.Name, err)
	}
	logger.Info("auth source built",
		"name", s.Name, "type", logType, "mode", "oidc", "issuer", issuer)
	return store, nil
}

