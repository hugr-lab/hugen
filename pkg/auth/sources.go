package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
	"github.com/hugr-lab/hugen/pkg/config"
)

// LoadFromView registers the named auth sources declared in the
// config.AuthView. Must be called after AddPrimary, since type=hugr
// entries become aliases on the primary Source.
//
// Subscribes to view.OnUpdate; phase-3 logs a warning when the
// subscription fires (live reload of auth sources lands in phase 6
// alongside Add/Remove APIs on Service).
func (s *Service) LoadFromView(ctx context.Context, view config.AuthView) error {
	if view == nil {
		return nil
	}
	if err := s.applySources(ctx, view.Sources()); err != nil {
		return err
	}
	view.OnUpdate(func() {
		s.logger.Warn("auth: live reload not implemented; restart hugen to apply config.auth changes")
	})
	return nil
}

func (s *Service) applySources(ctx context.Context, specs []config.AuthSource) error {
	for _, spec := range specs {
		if spec.Name == "" {
			return fmt.Errorf("auth: source has empty name")
		}
		switch strings.ToLower(spec.Type) {
		case "hugr":
			target := s.Primary()
			if target == "" {
				return fmt.Errorf("auth %q: type=hugr but no primary source registered", spec.Name)
			}
			if spec.Name == target {
				continue
			}
			if err := s.Alias(spec.Name, target); err != nil {
				return fmt.Errorf("auth %q: alias to %q: %w", spec.Name, target, err)
			}
			s.logger.Info("auth alias registered", "name", spec.Name, "target", target)

		case "oidc":
			if spec.Issuer == "" || spec.ClientID == "" {
				return fmt.Errorf("auth %q: type=oidc requires issuer + client_id", spec.Name)
			}
			src, err := s.buildOIDCSource(ctx, spec)
			if err != nil {
				return err
			}
			if err := s.Add(src); err != nil {
				return err
			}
			s.RegisterPromptLogin(src.PromptLogin)
			s.logger.Info("auth source built",
				"name", spec.Name, "type", "oidc", "issuer", spec.Issuer)

		default:
			return fmt.Errorf("auth %q: unsupported type %q (want hugr|oidc)", spec.Name, spec.Type)
		}
	}
	return nil
}

func (s *Service) buildOIDCSource(ctx context.Context, spec config.AuthSource) (*oidc.Source, error) {
	redirect := strings.TrimRight(s.baseURL, "/") + "/auth/callback"
	logger := slog.Default()
	if s.logger != nil {
		logger = s.logger
	}
	store, err := oidc.New(ctx, oidc.Config{
		Name:        spec.Name,
		IssuerURL:   spec.Issuer,
		ClientID:    spec.ClientID,
		RedirectURL: redirect,
		LoginPath:   spec.LoginPath,
		Logger:      logger.With("auth", spec.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("auth %q: %w", spec.Name, err)
	}
	return store, nil
}
